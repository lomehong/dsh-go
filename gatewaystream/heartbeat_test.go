package gatewaystream

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"dshgo/cordis"
	"dshgo/host/webserver"
)

// muxHarness wires one MuxServer behind an httptest HTTP server and dials a
// real WebSocket client over the loopback — the same upgrade path production
// uses (webserver registry upgrade route → hijack → mux).
type muxHarness struct {
	server *httptest.Server
	mux    *MuxServer
	url    string
}

func newMuxHarness(t *testing.T, interval time.Duration) (*muxHarness, *websocket.Conn) {
	t.Helper()
	mux := NewMuxServer(func(endpoint string, payload any) (<-chan any, <-chan error, func()) {
		ch := make(chan any)
		return ch, nil, func() {}
	}, cordis.Discard{}, interval)

	registry := webserver.New(cordis.Discard{})
	if err := mux.Register(registry); err != nil {
		t.Fatalf("register: %v", err)
	}
	server := httptest.NewServer(registry)
	t.Cleanup(func() {
		server.Close()
		_ = mux.Close()
	})
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + RemoteStreamMuxPath
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &muxHarness{server: server, mux: mux, url: wsURL}, conn
}

// stallPongs makes the client ignore server pings entirely (no pong frames).
func stallPongs(conn *websocket.Conn) {
	conn.SetPingHandler(func(string) error { return nil })
}

// autoPong makes the client answer every server ping immediately, like the
// Node ws autoPong the upstream tests rely on (gorilla does not pong by
// default).
func autoPong(conn *websocket.Conn) {
	conn.SetPingHandler(func(string) error {
		return conn.WriteControl(websocket.PongMessage, nil, time.Now().Add(time.Second))
	})
}

func TestB2HealthyClientSurvivesHeartbeats(t *testing.T) {
	// Auto-pong: the client answers every ping. It must stay alive well past
	// several heartbeat cycles.
	_, conn := newMuxHarness(t, 10*time.Millisecond)
	autoPong(conn)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
	time.Sleep(60 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("healthy client was terminated")
	default:
	}
}

func TestB2SilentClientTerminatedAfterMissedCap(t *testing.T) {
	h, conn := newMuxHarness(t, 10*time.Millisecond)
	stallPongs(conn)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
	// Two missed pings (20ms) → third tick schedules the deferred recheck →
	// terminate. Allow generous margin for scheduler timing.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("silent client was never terminated")
	}
	// The server must have dropped the socket from its live set.
	h.mux.mu.Lock()
	live := len(h.mux.connections)
	h.mux.mu.Unlock()
	if live != 0 {
		t.Fatalf("server still tracks %d connections after termination", live)
	}
}

func TestB2DataOnlyWithoutPongIsTerminated(t *testing.T) {
	_, conn := newMuxHarness(t, 10*time.Millisecond)
	// The client sends data frames (activity) but never pongs: the
	// heartbeat must still count it stalled — data activity is not pong
	// evidence (upstream heartbeatAlive counts pongs only).
	conn.SetPingHandler(func(string) error { return nil })
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if err := conn.WriteMessage(websocket.TextMessage, []byte("busy")); err != nil {
				return
			}
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("data-only client was never terminated")
	}
}

func TestB2DelayedPongBeforeFinalCheckKeepsSocket(t *testing.T) {
	// Pure state-machine test (no live heartbeat timing): a socket whose
	// missed counter crossed the cap schedules the deferred finalize; a pong
	// clearing the counter before the final check runs must cancel the
	// termination. The deterministic scheduler removes all load-sensitive
	// timing from the assertion.
	h, conn := newMuxHarness(t, time.Hour) // heartbeat effectively off
	var captured func()
	h.mux.SetFinalizeScheduler(func(fn func()) { captured = fn })

	h.mux.mu.Lock()
	h.mux.missed[conn] = maxMissedHeartbeats
	h.mux.mu.Unlock()
	go h.mux.terminateIfStillStalled(conn, maxMissedHeartbeats)
	deadline := time.Now().Add(2 * time.Second)
	for captured == nil && time.Now().Before(deadline) {
		time.Sleep(1 * time.Millisecond)
	}
	if captured == nil {
		t.Fatal("deferred finalize was never scheduled")
	}

	// The pong arrives before the final check: the counter clears and the
	// finalize must not terminate the socket.
	h.mux.mu.Lock()
	h.mux.missed[conn] = 0
	h.mux.mu.Unlock()
	captured()

	h.mux.mu.Lock()
	live := len(h.mux.connections)
	h.mux.mu.Unlock()
	if live != 1 {
		t.Fatalf("live = %d, want 1 (pong before the final check must cancel termination)", live)
	}
}

func TestB2CloseDuringPendingTerminateNoLeak(t *testing.T) {
	h, conn := newMuxHarness(t, 10*time.Millisecond)
	stallPongs(conn)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
	// Let the missed counter exceed the cap (pending terminate), then Close
	// the server: no panic, no double-close, no leaked connection set.
	time.Sleep(40 * time.Millisecond)
	_ = h.mux.Close()
	time.Sleep(20 * time.Millisecond)
	h.mux.mu.Lock()
	live := len(h.mux.connections)
	missed := len(h.mux.missed)
	h.mux.mu.Unlock()
	if live != 0 || missed != 0 {
		t.Fatalf("after Close: live=%d missed=%d, want 0/0", live, missed)
	}
}

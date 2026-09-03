// Package gatewaystream ports packages/api/gateway/src/{stream-protocol,stream-server}.ts:
// the Host WebSocket mux owner for multiplexed Typert Remote streams.
package gatewaystream

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"dshgo/cordis"
	"dshgo/host/webserver"
)

// RemoteStreamOpener opens one validated Remote stream for a decoded wire
// request; each yielded value is one server frame payload.
type RemoteStreamOpener func(endpoint string, payload any) (<-chan any, <-chan error, func())

// maxMissedHeartbeats is the stalled-host tolerance before termination:
// two consecutive unanswered pings (upstream MAX_MISSED_HEARTBEATS).
const maxMissedHeartbeats = 2

// defaultHeartbeatInterval is the ping cadence when the caller passes zero.
const defaultHeartbeatInterval = 20 * time.Second

// MuxServer owns the no-server WebSocket acceptor and every active logical
// stream, mounted as one upgrade route on the host webserver. It runs one
// shared heartbeat timer (started on the first upgrade, spanning empty-client
// periods) that pings every open socket and terminates sockets that miss
// maxMissedHeartbeats consecutive pings — with the upstream stalled-host
// tolerance: a socket is only terminated when the missed count still exceeds
// the cap at a deferred recheck, so a delayed pong can clear it first.
type MuxServer struct {
	upgrader websocket.Upgrader
	open     RemoteStreamOpener
	logger   cordis.Logger
	interval time.Duration

	mu          sync.Mutex
	connections map[*websocket.Conn]struct{}
	missed      map[*websocket.Conn]int
	streams     map[string]func()
	closed      bool
	hbStarted   bool
	hbStop      chan struct{}
	schedule    finalizeScheduler
}

// NewMuxServer builds the mux owner. The opener is the Gateway stream
// dispatcher. heartbeatInterval is the ping cadence; zero selects the
// default (20s, matching the upstream default argument).
func NewMuxServer(open RemoteStreamOpener, logger cordis.Logger, heartbeatInterval time.Duration) *MuxServer {
	if heartbeatInterval <= 0 {
		heartbeatInterval = defaultHeartbeatInterval
	}
	return &MuxServer{
		upgrader: websocket.Upgrader{
			HandshakeTimeout: 10 * time.Second,
			// The trusted Host accepts any origin; the client is the paired
			// web app over the authenticated upgrade path.
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		open:        open,
		logger:      logger,
		interval:    heartbeatInterval,
		connections: map[*websocket.Conn]struct{}{},
		missed:      map[*websocket.Conn]int{},
		streams:     map[string]func(){},
	}
}

// Register mounts the mux on the webserver upgrade registry.
func (s *MuxServer) Register(registry *webserver.Registry) error {
	_, err := registry.RegisterUpgrade(webserver.UpgradeRoute{
		Path: RemoteStreamMuxPath,
		Handler: func(conn net.Conn, rw *bufio.ReadWriter, r *http.Request) error {
			return s.handleUpgrade(conn, rw, r)
		},
	})
	return err
}

// handleUpgrade adapts the hijacked connection into an HTTP 101 response
// writer and begins serving its logical streams.
func (s *MuxServer) handleUpgrade(conn net.Conn, rw *bufio.ReadWriter, r *http.Request) error {
	responseWriter := &hijackResponseWriter{conn: conn, rw: rw, header: http.Header{}}
	ws, err := s.upgrader.Upgrade(responseWriter, r, nil)
	if err != nil {
		return fmt.Errorf("remote mux upgrade: %w", err)
	}
	ws.SetPongHandler(func(string) error {
		s.mu.Lock()
		s.missed[ws] = 0
		s.mu.Unlock()
		return nil
	})
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = ws.Close()
		return nil
	}
	s.connections[ws] = struct{}{}
	s.missed[ws] = 0
	s.startHeartbeatLocked()
	s.mu.Unlock()
	// Run the connection synchronously: the webserver registry closes the
	// hijacked socket when the upgrade handler returns, so returning here
	// while runConnection still reads would kill the socket under it. The
	// registry's deferred Close then fires after the connection ends —
	// idempotent and harmless.
	s.runConnection(ws)
	return nil
}

// startHeartbeatLocked starts the single shared heartbeat timer on the first
// upgrade; it spans empty-client periods until Close (upstream startHeartbeat).
func (s *MuxServer) startHeartbeatLocked() {
	if s.hbStarted {
		return
	}
	s.hbStarted = true
	s.hbStop = make(chan struct{})
	go s.heartbeatLoop()
}

// heartbeatLoop pings every open socket on the cadence and terminates
// stalled ones with the upstream tolerance.
func (s *MuxServer) heartbeatLoop() {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.heartbeatTick()
		case <-s.hbStop:
			return
		}
	}
}

// heartbeatTick advances the missed counter for every open socket and
// terminates sockets over the cap — through a deferred recheck so a delayed
// pong arriving before the final check clears the count (upstream
// setImmediate recheck). Network I/O (ping write) happens outside the lock:
// a stalled peer's full TCP buffer must not stall the whole mux.
func (s *MuxServer) heartbeatTick() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	type stalled struct {
		ws     *websocket.Conn
		missed int
	}
	targets := make([]stalled, 0, len(s.connections))
	pings := make([]*websocket.Conn, 0, len(s.connections))
	for ws := range s.connections {
		if s.missed[ws] >= maxMissedHeartbeats {
			targets = append(targets, stalled{ws: ws, missed: s.missed[ws]})
			continue
		}
		s.missed[ws]++
		pings = append(pings, ws)
	}
	s.mu.Unlock()

	for _, ws := range pings {
		_ = ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second))
	}
	for _, target := range targets {
		go s.terminateIfStillStalled(target.ws, target.missed)
	}
}

// scheduleFinalize schedules the deferred missed-count recheck; production
// uses a real goroutine delay, tests substitute a deterministic driver via
// SetFinalizeScheduler.
type finalizeScheduler func(func())

// finalCheckDelay is the grace window between the missed-cap check and the
// termination itself — the Go equivalent of the upstream setImmediate
// recheck. It gives a pong that is already in flight time to arrive and
// reset the counter before the socket is killed.
const finalCheckDelay = 5 * time.Millisecond

// finalize schedules the final recheck through the configured scheduler.
func (s *MuxServer) finalize(fn func()) {
	if s.schedule != nil {
		s.schedule(fn)
		return
	}
	time.Sleep(finalCheckDelay)
	fn()
}

// SetFinalizeScheduler installs a deterministic scheduler for the deferred
// missed-count recheck (test seam; the upstream setImmediate mock analogue).
// nil restores the production goroutine+delay path.
func (s *MuxServer) SetFinalizeScheduler(schedule finalizeScheduler) {
	s.mu.Lock()
	s.schedule = schedule
	s.mu.Unlock()
}

// terminateIfStillStalled rechecks the missed count before terminating: the
// deferred recheck (finalCheckDelay) gives a just-arrived pong the chance to
// reset the counter, so a host that merely stalls one beat longer than the
// cadence survives.
func (s *MuxServer) terminateIfStillStalled(ws *websocket.Conn, observed int) {
	s.finalize(func() {
		s.mu.Lock()
		if s.closed || s.missed[ws] != observed || s.missed[ws] < maxMissedHeartbeats {
			s.mu.Unlock()
			return
		}
		delete(s.missed, ws)
		delete(s.connections, ws)
		s.mu.Unlock()
		_ = ws.Close()
	})
}

// wsWriter serializes data-frame writes on one socket: gorilla/websocket
// permits at most one concurrent writer, and each open stream relays from
// its own goroutine.
type wsWriter struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func (w *wsWriter) writeJSON(v any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conn.WriteJSON(v)
}

// runConnection reads client messages (open/close frames) and multiplexes
// the logical streams until the socket dies.
func (s *MuxServer) runConnection(ws *websocket.Conn) {
	writer := &wsWriter{conn: ws}
	defer func() {
		s.cancelAll()
		s.mu.Lock()
		delete(s.connections, ws)
		delete(s.missed, ws)
		s.mu.Unlock()
		_ = ws.Close()
	}()
	for {
		_, message, err := ws.ReadMessage()
		if err != nil {
			return
		}
		clientMessage, parseErr := parseClientMessage(message)
		if parseErr != nil {
			// The official server closes malformed-JSON sockets (1008)
			// instead of answering them.
			deadline := time.Now().Add(time.Second)
			_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1008, parseErr.Error()), deadline)
			return
		}
		switch clientMessage.Kind {
		case "open":
			s.openStream(writer, clientMessage)
		case "cancel":
			s.cancelStream(clientMessage.StreamID)
		}
	}
}

// openStream dispatches one logical stream for an open request and relays
// its frames to the client. Frames ride the official item envelope
// ({type:'item', streamId, value}); stream errors surface as error frames;
// source exhaustion answers end. The cancel is tracked per stream so a
// client cancel message (or the socket dying) tears the source down.
func (s *MuxServer) openStream(w *wsWriter, msg clientMessage) {
	if s.open == nil {
		_ = w.writeJSON(map[string]any{"type": "error", "streamId": msg.StreamID, "error": map[string]any{"code": "internal", "message": "remote stream dispatcher is not mounted"}})
		return
	}
	frames, errs, cancel := s.open(msg.Endpoint, msg.Payload)
	s.mu.Lock()
	s.streams[msg.StreamID] = cancel
	s.mu.Unlock()
	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.streams, msg.StreamID)
			s.mu.Unlock()
			if cancel != nil {
				cancel()
			}
		}()
		for {
			select {
			case frame, ok := <-frames:
				if !ok {
					_ = w.writeJSON(map[string]any{"type": "end", "streamId": msg.StreamID})
					return
				}
				_ = w.writeJSON(map[string]any{"type": "item", "streamId": msg.StreamID, "value": frame})
			case err, ok := <-errs:
				if !ok {
					return
				}
				_ = w.writeJSON(map[string]any{"type": "error", "streamId": msg.StreamID, "error": map[string]any{"code": "internal", "message": err.Error()}})
				return
			}
		}
	}()
}

// cancelStream tears one tracked stream down on the client's cancel message.
func (s *MuxServer) cancelStream(streamID string) {
	s.mu.Lock()
	cancel, tracked := s.streams[streamID]
	if tracked {
		delete(s.streams, streamID)
	}
	s.mu.Unlock()
	if tracked && cancel != nil {
		cancel()
	}
}

// cancelAll tears every tracked stream down when the owning socket dies.
func (s *MuxServer) cancelAll() {
	s.mu.Lock()
	streams := s.streams
	s.streams = map[string]func(){}
	s.mu.Unlock()
	for _, cancel := range streams {
		if cancel != nil {
			cancel()
		}
	}
}

// Close terminates all sockets and stops the shared heartbeat timer, then
// waits until every iterator has returned.
func (s *MuxServer) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	if s.hbStarted {
		close(s.hbStop)
	}
	conns := make([]*websocket.Conn, 0, len(s.connections))
	for conn := range s.connections {
		conns = append(conns, conn)
	}
	s.connections = map[*websocket.Conn]struct{}{}
	s.missed = map[*websocket.Conn]int{}
	s.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
	return nil
}

// hijackResponseWriter adapts an already-hijacked connection to the
// http.ResponseWriter contract gorilla/websocket's Upgrader needs to write
// the 101 handshake.
type hijackResponseWriter struct {
	conn   net.Conn
	rw     *bufio.ReadWriter
	header http.Header
}

func (h *hijackResponseWriter) Header() http.Header { return h.header }
func (h *hijackResponseWriter) WriteHeader(statusCode int) {
	// The Upgrader writes the full 101 response through Write; a non-101
	// status is written inline for error paths.
	if statusCode != http.StatusSwitchingProtocols {
		fmt.Fprintf(h.rw, "HTTP/1.1 %d %s\r\n", statusCode, http.StatusText(statusCode))
		h.header.Write(h.rw)
		fmt.Fprint(h.rw, "\r\n")
		_ = h.rw.Flush()
	}
}
func (h *hijackResponseWriter) Write(b []byte) (int, error) {
	_, _ = h.rw.Write(b)
	_ = h.rw.Flush()
	return len(b), nil
}
func (h *hijackResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return h.conn, h.rw, nil
}

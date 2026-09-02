package gatewaystream

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"time"

	"dshgo/cordis"
	"dshgo/host/webserver"

	"github.com/gorilla/websocket"
)

// LegacyDownlink accepts the pre-unification downlink-only event sockets
// (api-path MUX_EVENTS_PATH / HOST_EVENTS_PATH: /api/events.mux,
// /api/events.host). The old browser bundle opens both right after the page
// loads and treats "both open" as half of the connection-readiness gate;
// it never sends application data and only consumes downstream frames.
// The Go host runs with an empty session catalog, so the sockets are held
// open with zero backfill — the honest empty state.
type LegacyDownlink struct {
	upgrader websocket.Upgrader
}

// NewLegacyDownlink builds the legacy downlink acceptor.
func NewLegacyDownlink() *LegacyDownlink {
	return &LegacyDownlink{
		upgrader: websocket.Upgrader{
			HandshakeTimeout: 10 * time.Second,
			CheckOrigin:      func(r *http.Request) bool { return true },
		},
	}
}

// Register mounts one upgrade route per legacy path.
func (s *LegacyDownlink) Register(registry *webserver.Registry, logger cordis.Logger, paths ...string) error {
	for _, path := range paths {
		path := path
		if _, err := registry.RegisterUpgrade(webserver.UpgradeRoute{
			Path: path,
			Handler: func(conn net.Conn, rw *bufio.ReadWriter, r *http.Request) error {
				return s.handleUpgrade(conn, rw, r, path, logger)
			},
		}); err != nil {
			return err
		}
	}
	return nil
}

// handleUpgrade accepts the socket and holds it open, downlink-only: any
// client-to-server data message closes with 1008 (official semantics);
// control frames (ping/pong) are handled inside the read pump.
func (s *LegacyDownlink) handleUpgrade(conn net.Conn, rw *bufio.ReadWriter, r *http.Request, path string, logger cordis.Logger) error {
	responseWriter := &hijackResponseWriter{conn: conn, rw: rw, header: http.Header{}}
	ws, err := s.upgrader.Upgrade(responseWriter, r, nil)
	if err != nil {
		return fmt.Errorf("legacy downlink %s upgrade: %w", path, err)
	}
	for {
		messageType, _, err := ws.ReadMessage()
		if err != nil {
			return nil
		}
		if messageType == websocket.TextMessage || messageType == websocket.BinaryMessage {
			deadline := time.Now().Add(time.Second)
			_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1008, "downlink only"), deadline)
			return nil
		}
	}
}

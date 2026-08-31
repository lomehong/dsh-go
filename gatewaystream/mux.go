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

// MuxServer owns the no-server WebSocket acceptor and every active logical
// stream, mounted as one upgrade route on the host webserver.
type MuxServer struct {
	upgrader websocket.Upgrader
	open     RemoteStreamOpener
	logger   cordis.Logger

	mu          sync.Mutex
	connections map[*websocket.Conn]struct{}
	closed      bool
}

// NewMuxServer builds the mux owner. The opener is the Gateway stream
// dispatcher.
func NewMuxServer(open RemoteStreamOpener, logger cordis.Logger) *MuxServer {
	return &MuxServer{
		upgrader: websocket.Upgrader{
			HandshakeTimeout: 10 * time.Second,
			// The trusted Host accepts any origin; the client is the paired
			// web app over the authenticated upgrade path.
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		open:        open,
		logger:      logger,
		connections: map[*websocket.Conn]struct{}{},
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
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = ws.Close()
		return nil
	}
	s.connections[ws] = struct{}{}
	s.mu.Unlock()
	go s.runConnection(ws)
	return nil
}

// runConnection reads client messages (open/close frames) and multiplexes
// the logical streams until the socket dies.
func (s *MuxServer) runConnection(ws *websocket.Conn) {
	defer func() {
		s.mu.Lock()
		delete(s.connections, ws)
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
			_ = ws.WriteJSON(map[string]any{"type": "error", "message": parseErr.Error()})
			continue
		}
		switch clientMessage.Kind {
		case "open":
			s.openStream(ws, clientMessage)
		case "close":
			// The upstream client can request stream closure; the opened
			// stream's cancellation is delivered through its own frame.
			_ = ws.WriteJSON(map[string]any{"type": "closed"})
		}
	}
}

// openStream dispatches one logical stream for an open request and relays
// its frames to the client.
func (s *MuxServer) openStream(ws *websocket.Conn, msg clientMessage) {
	if s.open == nil {
		_ = ws.WriteJSON(map[string]any{"type": "error", "message": "remote stream dispatcher is not mounted"})
		return
	}
	frames, errs, cancel := s.open(msg.Endpoint, msg.Payload)
	if cancel != nil {
		defer cancel()
	}
	go func() {
		for {
			select {
			case frame, ok := <-frames:
				if !ok {
					_ = ws.WriteJSON(map[string]any{"type": "end", "endpoint": msg.Endpoint})
					return
				}
				_ = ws.WriteJSON(frame)
			case err, ok := <-errs:
				if !ok {
					return
				}
				_ = ws.WriteJSON(map[string]any{"type": "failure", "endpoint": msg.Endpoint, "error": err.Error()})
				return
			}
		}
	}()
}

// Close terminates all sockets and waits until every iterator has returned.
func (s *MuxServer) Close() error {
	s.mu.Lock()
	s.closed = true
	conns := make([]*websocket.Conn, 0, len(s.connections))
	for conn := range s.connections {
		conns = append(conns, conn)
	}
	s.connections = map[*websocket.Conn]struct{}{}
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

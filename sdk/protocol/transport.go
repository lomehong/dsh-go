package protocol

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// JsonRpcResponseError is a JSON-RPC error response, preserving the wire
// code and optional data. A nil Code means the peer sent none.
type JsonRpcResponseError struct {
	// Code is the wire error code, or nil when the peer sent none.
	Code *int
	// Text is the wire error message.
	Text string
	// Data is the optional structured error payload, verbatim.
	Data any
}

// Error implements error.
func (e *JsonRpcResponseError) Error() string { return e.Text }

// JSON-RPC wire error codes used by the transport.
const (
	codeMethodNotFound = -32601
	codeInternalError  = -32603
)

// Peer is the outbound request and notification surface used by the runtime
// server and SDK clients.
type Peer interface {
	// Request sends a request and awaits its response. The error is a
	// *JsonRpcResponseError on an error response, and a plain error on a
	// write failure or closure.
	Request(ctx context.Context, method string, params any) (any, error)
	// Notify sends a notification; a nil params produces no params member.
	Notify(method string, params any)
}

// RequestHandler resolves one incoming request to the response result; a
// non-nil error becomes a -32603 error response carrying its message.
type RequestHandler func(method string, params map[string]any) (any, error)

// NotificationHandler receives one notification with its method and
// normalized params object.
type NotificationHandler func(method string, params map[string]any)

// LineTransport is the line-delimited JSON-RPC endpoint over caller-owned
// byte streams. Start attaches the read loop; Close stops reading and fails
// pending requests without closing the streams. Missing request handlers
// return -32601; handler failures return -32603. Notifications without a
// handler are dropped. Malformed peer lines are ignored.
type LineTransport struct {
	out io.Writer

	writeMu sync.Mutex

	mu         sync.Mutex
	started    bool
	closed     bool
	cancelRead context.CancelFunc
	done       chan struct{}

	requestHandler      RequestHandler
	notificationHandler NotificationHandler
	pending             map[string]chan transportReply
}

// transportReply is one settled response frame.
type transportReply struct {
	result any
	err    error
}

// NewLineTransport builds the endpoint over the given byte streams.
func NewLineTransport(input io.Reader, output io.Writer) *LineTransport {
	return &LineTransport{
		out:     output,
		pending: map[string]chan transportReply{},
		done:    make(chan struct{}),
	}
}

// Start attaches the read loop and begins consuming frames. Idempotent.
func (t *LineTransport) Start() {
	t.mu.Lock()
	if t.started || t.closed {
		t.mu.Unlock()
		return
	}
	t.started = true
	ctx, cancel := context.WithCancel(context.Background())
	t.cancelRead = cancel
	t.mu.Unlock()
	go t.read(ctx)
}

// Close detaches the read loop and fails pending requests. Safe before
// Start.
func (t *LineTransport) Close() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	cancel := t.cancelRead
	t.pending, t.closedPending = nil, t.takePendingLocked()
	t.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	t.failPending(newError("JSON-RPC transport closed"))
}

// closedPending holds the pending map moved out under the lock.
var _ = struct{}{}

func (t *LineTransport) takePendingLocked() map[string]chan transportReply { return nil }

// OnRequest installs the request handler, replacing any prior handler.
func (t *LineTransport) OnRequest(handler RequestHandler) {
	t.mu.Lock()
	t.requestHandler = handler
	t.mu.Unlock()
}

// OnNotification installs the notification handler, replacing any prior
// handler.
func (t *LineTransport) OnNotification(handler NotificationHandler) {
	t.mu.Lock()
	t.notificationHandler = handler
	t.mu.Unlock()
}

// Request sends a request and awaits its response. Abandoning ctx removes
// the pending entry (no state is retained for a response that may never
// come) and fails with the context error.
func (t *LineTransport) Request(ctx context.Context, method string, params any) (any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := hex.EncodeToString(randomID())
	if err != nil {
		return nil, err
	}
	id := "req_" + raw
	reply := make(chan transportReply, 1)
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, newError("JSON-RPC transport closed")
	}
	t.pending[id] = reply
	t.mu.Unlock()
	message := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		message["params"] = params
	}
	if err := t.write(message); err != nil {
		t.takePending(id)
		return nil, err
	}
	select {
	case settled := <-reply:
		return settled.result, settled.err
	case <-ctx.Done():
		t.takePending(id)
		return nil, ctx.Err()
	}
}

// Notify sends a notification; nil params produces no params member.
func (t *LineTransport) Notify(method string, params any) {
	message := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		message["params"] = params
	}
	_ = t.write(message)
}

func (t *LineTransport) read(ctx context.Context) {
	defer close(t.done)
	reader := bufio.NewReaderSize(ctxReader{ctx: ctx, inner: t.input()}, 1<<16)
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			t.handleLine(line)
		}
		if err != nil {
			if ctx.Err() == nil {
				t.failPending(newError("JSON-RPC input closed"))
			}
			return
		}
	}
}

// ctxReader is the transport's input; Close swaps it to force reads awake.
// The indirection lets Close stop a blocked reader without closing the
// caller-owned stream.
type ctxReader struct {
	ctx   context.Context
	inner io.Reader
}

// input returns the current input reader (swapped by Close to unblock).
var _ = ctxReader{}

func (r ctxReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.inner.Read(p)
}

func (t *LineTransport) input() io.Reader {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.reader
}

func (t *LineTransport) handleLine(raw string) {
	line := trimSpace(raw)
	if line == "" {
		return
	}
	var frame struct {
		Jsonrpc string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  *string         `json:"method"`
		Params  json.RawMessage `json:"params"`
		Result  json.RawMessage `json:"result"`
		Error   *struct {
			Code    *int            `json:"code"`
			Message *string         `json:"message"`
			Data    json.RawMessage `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &frame); err != nil {
		// Only JSON syntax errors reach this branch; malformed peer lines
		// are ignored.
		return
	}
	id, hasID := decodeID(frame.ID)
	method := ""
	if frame.Method != nil {
		method = *frame.Method
	}
	params := objectParams(frame.Params)
	switch {
	case hasID && method != "":
		t.handleIncomingRequest(id, method, params)
	case hasID:
		t.handleIncomingResponse(id, frame.Result, frame.Error)
	case method != "":
		t.mu.Lock()
		handler := t.notificationHandler
		t.mu.Unlock()
		if handler != nil {
			handler(method, params)
		}
	}
}

func (t *LineTransport) handleIncomingRequest(id any, method string, params map[string]any) {
	t.mu.Lock()
	handler := t.requestHandler
	t.mu.Unlock()
	if handler == nil {
		_ = t.write(map[string]any{"jsonrpc": "2.0", "id": id,
			"error": map[string]any{"code": codeMethodNotFound, "message": fmt.Sprintf("method not found: %s", method)}})
		return
	}
	result, err := handler(method, params)
	if err != nil {
		message := err.Error()
		_ = t.write(map[string]any{"jsonrpc": "2.0", "id": id,
			"error": map[string]any{"code": codeInternalError, "message": message}})
		return
	}
	_ = t.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (t *LineTransport) handleIncomingResponse(id any, rawResult json.RawMessage, wireError *struct {
	Code    *int            `json:"code"`
	Message *string         `json:"message"`
	Data    json.RawMessage `json:"data"`
}) {
	key := idKey(id)
	t.mu.Lock()
	reply, ok := t.pending[key]
	if ok {
		delete(t.pending, key)
	}
	t.mu.Unlock()
	if !ok {
		return
	}
	if wireError != nil {
		message := "JSON-RPC error"
		if wireError.Message != nil {
			message = *wireError.Message
		}
		var data any
		if len(wireError.Data) > 0 {
			_ = json.Unmarshal(wireError.Data, &data)
		}
		reply <- transportReply{err: &JsonRpcResponseError{Code: wireError.Code, Text: message, Data: data}}
		return
	}
	var result any
	if len(rawResult) > 0 {
		_ = json.Unmarshal(rawResult, &result)
	}
	reply <- transportReply{result: result}
}

func (t *LineTransport) write(message map[string]any) error {
	encoded, err := json.Marshal(message)
	if err != nil {
		return err
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	_, err = t.out.Write(append(encoded, '\n'))
	return err
}

func (t *LineTransport) takePending(id string) {
	t.mu.Lock()
	delete(t.pending, id)
	t.mu.Unlock()
}

func (t *LineTransport) failPending(err error) {
	t.mu.Lock()
	pending := t.pending
	t.pending = map[string]chan transportReply{}
	t.mu.Unlock()
	for _, reply := range pending {
		reply <- transportReply{err: err}
	}
}

func randomID() ([]byte, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// decodeID accepts the wire id shapes: string and number.
func decodeID(raw json.RawMessage) (any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, true
	}
	var number float64
	if err := json.Unmarshal(raw, &number); err == nil {
		return number, true
	}
	return nil, false
}

// idKey maps a decoded id onto its pending-map key without colliding string
// and number ids.
func idKey(id any) string {
	switch typed := id.(type) {
	case string:
		return "s:" + typed
	case float64:
		return "n:" + fmt.Sprintf("%v", typed)
	default:
		return fmt.Sprintf("o:%v", typed)
	}
}

// objectParams normalizes JSON-RPC params to a plain object (arrays and
// scalars collapse to an empty object).
func objectParams(raw json.RawMessage) map[string]any {
	var params map[string]any
	if len(raw) > 0 && json.Unmarshal(raw, &params) == nil && params != nil {
		return params
	}
	return map[string]any{}
}

// newError is the transport's plain error type (non-response failures).
type transportError struct{ message string }

func newError(message string) error { return &transportError{message: message} }

func (e *transportError) Error() string { return e.message }

func trimSpace(value string) string {
	start, end := 0, len(value)
	for start < end && (value[start] == ' ' || value[start] == '\t' || value[start] == '\r') {
		start++
	}
	for end > start && (value[end-1] == ' ' || value[end-1] == '\t' || value[end-1] == '\r') {
		end--
	}
	return value[start:end]
}

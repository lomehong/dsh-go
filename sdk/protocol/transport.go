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
// byte streams. Start attaches the read loop; Close stops frame handling and
// fails pending requests without closing the streams. Missing request
// handlers return -32601; handler failures return -32603. Notifications
// without a handler are dropped. Malformed peer lines are ignored.
//
// Go adaptation of the evented source: the read goroutine parks inside the
// caller's blocking Read, so Close does not wake it — it exits at the
// stream's next EOF or error, and frames arriving after Close are dropped.
type LineTransport struct {
	input  io.Reader
	out    io.Writer
	writer *bufio.Writer

	writeMu sync.Mutex

	mu                  sync.Mutex
	started             bool
	closed              bool
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
		input:   input,
		out:     output,
		writer:  bufio.NewWriter(output),
		pending: map[string]chan transportReply{},
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
	t.mu.Unlock()
	go t.read()
}

// Close stops frame handling and fails pending requests. Safe before Start.
func (t *LineTransport) Close() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	pending := t.pending
	t.pending = map[string]chan transportReply{}
	t.mu.Unlock()
	for _, reply := range pending {
		reply <- transportReply{err: plainError("JSON-RPC transport closed")}
	}
}

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
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	id := "req_" + hex.EncodeToString(raw)
	// Register under the same namespaced key the response path derives, so
	// string and number ids never collide.
	idMapKey := idKey(id)
	reply := make(chan transportReply, 1)
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, plainError("JSON-RPC transport closed")
	}
	t.pending[idMapKey] = reply
	t.mu.Unlock()
	message := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		message["params"] = params
	}
	if err := t.write(message); err != nil {
		t.mu.Lock()
		delete(t.pending, idMapKey)
		t.mu.Unlock()
		return nil, err
	}
	select {
	case settled := <-reply:
		return settled.result, settled.err
	case <-ctx.Done():
		t.mu.Lock()
		delete(t.pending, idMapKey)
		t.mu.Unlock()
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

// Flush waits for prior frame writes to reach the output stream.
func (t *LineTransport) Flush() error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	return t.writer.Flush()
}

// maxFrameBytes caps one JSON-RPC line: the read loop would otherwise grow
// without bound on a newline-less peer. The cap clears the attachment
// pipeline's worst-case base64 image frame (~5.5 MiB) with headroom.
const maxFrameBytes = 16 << 20

// readFrameLine reads one newline-terminated frame under the frame cap. A
// partial tail line comes back alongside its error, preserving ReadString
// semantics for a peer that closes mid-line.
func readFrameLine(reader *bufio.Reader) (string, error) {
	var line []byte
	for {
		chunk, isPrefix, err := reader.ReadLine()
		line = append(line, chunk...)
		if err != nil {
			if len(line) <= maxFrameBytes {
				return string(line), err
			}
			return "", err
		}
		if !isPrefix {
			return string(line), nil
		}
		if len(line) > maxFrameBytes {
			return "", fmt.Errorf("jsonrpc: frame exceeds the %d-byte cap", maxFrameBytes)
		}
	}
}

func (t *LineTransport) read() {
	reader := bufio.NewReaderSize(t.input, 1<<16)
	for {
		line, err := readFrameLine(reader)
		if len(line) > 0 {
			t.handleLine(line)
		}
		if err != nil {
			t.mu.Lock()
			closed := t.closed
			t.mu.Unlock()
			if !closed {
				t.failPending(plainError("JSON-RPC input closed"))
			}
			return
		}
	}
}

func (t *LineTransport) handleLine(raw string) {
	line := trimSpace(raw)
	if line == "" {
		return
	}
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return
	}
	var frame struct {
		ID     json.RawMessage `json:"id"`
		Method *string         `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
		Error  *wireErrorFrame `json:"error"`
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

// wireErrorFrame is the error member of a response frame.
type wireErrorFrame struct {
	Code    *int            `json:"code"`
	Message *string         `json:"message"`
	Data    json.RawMessage `json:"data"`
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
		_ = t.write(map[string]any{"jsonrpc": "2.0", "id": id,
			"error": map[string]any{"code": codeInternalError, "message": err.Error()}})
		return
	}
	_ = t.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (t *LineTransport) handleIncomingResponse(id any, rawResult json.RawMessage, wireError *wireErrorFrame) {
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
	if _, err := t.writer.Write(encoded); err != nil {
		return err
	}
	if err := t.writer.WriteByte('\n'); err != nil {
		return err
	}
	return t.writer.Flush()
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

// decodeID accepts the wire id shapes: string and number. JSON null
// unmarshals into any target, so it is rejected before the shape probes.
func decodeID(raw json.RawMessage) (any, bool) {
	if len(raw) == 0 || string(raw) == "null" {
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

// plainError is the transport's plain error type (non-response failures).
type plainErrorString struct{ message string }

func plainError(message string) error { return &plainErrorString{message: message} }

func (e *plainErrorString) Error() string { return e.message }

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

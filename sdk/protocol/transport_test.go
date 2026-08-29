package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// wirePair connects two transports over in-process pipes, mirroring the
// stdio pairing: client output is server input and vice versa.
func wirePair(t *testing.T) (*LineTransport, *LineTransport) {
	t.Helper()
	clientToServerReader, clientToServerWriter := io.Pipe()
	serverToClientReader, serverToClientWriter := io.Pipe()
	client := NewLineTransport(clientToServerReader, serverToClientWriter)
	server := NewLineTransport(serverToClientReader, clientToServerWriter)
	client.Start()
	server.Start()
	t.Cleanup(func() {
		client.Close()
		server.Close()
	})
	return client, server
}

func TestRequestResponseRoundTrip(t *testing.T) {
	client, server := wirePair(t)
	server.OnRequest(func(method string, params map[string]any) (any, error) {
		if method != "session/prompt" {
			return nil, errors.New("unexpected method")
		}
		return map[string]any{"messageId": params["sessionId"]}, nil
	})
	result, err := client.Request(context.Background(), "session/prompt",
		map[string]any{"sessionId": "s-1", "contentBlocks": []any{}})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resultMap := result.(map[string]any)
	if resultMap["messageId"] != "s-1" {
		t.Fatalf("result = %#v", result)
	}
}

func TestHandlerFailureBecomesInternalError(t *testing.T) {
	client, server := wirePair(t)
	server.OnRequest(func(string, map[string]any) (any, error) {
		return nil, errors.New("provider exploded")
	})
	_, err := client.Request(context.Background(), "initialize", map[string]any{})
	var rpcErr *JsonRpcResponseError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("err = %v, want a JsonRpcResponseError", err)
	}
	if rpcErr.Code == nil || *rpcErr.Code != codeInternalError || rpcErr.Text != "provider exploded" {
		t.Fatalf("rpcErr = %+v", rpcErr)
	}
}

func TestMethodNotFoundWithoutHandler(t *testing.T) {
	client, _ := wirePair(t)
	_, err := client.Request(context.Background(), "shutdown", nil)
	var rpcErr *JsonRpcResponseError
	if !errors.As(err, &rpcErr) || rpcErr.Code == nil || *rpcErr.Code != codeMethodNotFound ||
		rpcErr.Text != "method not found: shutdown" {
		t.Fatalf("err = %v", err)
	}
}

func TestErrorResponseCarriesCodeMessageData(t *testing.T) {
	// Script the error decoding directly: a response frame with code,
	// message, and a structured data payload.
	script := `{"jsonrpc":"2.0","id":"req_x","method":"probe"}` + "\n"
	in, inWriter := io.Pipe()
	out := &captureWriter{}
	transport := NewLineTransport(in, out)
	transport.Start()
	reply := make(chan error, 1)
	go func() {
		_, err := transport.Request(context.Background(), "probe", nil)
		reply <- err
	}()
	waitFor(t, out, `"method":"probe"`)
	if _, err := inWriter.Write([]byte(
		`{"jsonrpc":"2.0","id":"` + lastRequestID(t, out) + `","error":{"code":-32000,"message":"admission refused","data":{"block":2}}}` + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	var rpcErr *JsonRpcResponseError
	select {
	case err := <-reply:
		if !errors.As(err, &rpcErr) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("error response never settled the request")
	}
	if rpcErr.Code == nil || *rpcErr.Code != -32000 || rpcErr.Text != "admission refused" {
		t.Fatalf("rpcErr = %+v", rpcErr)
	}
	data, ok := rpcErr.Data.(map[string]any)
	if !ok || data["block"] != 2.0 {
		t.Fatalf("data = %#v", rpcErr.Data)
	}
	_ = script
}

// lastRequestID reads the single request frame's id from the capture.
func lastRequestID(t *testing.T, out *captureWriter) string {
	t.Helper()
	text := out.String()
	start := strings.Index(text, `"id":"`)
	if start < 0 {
		t.Fatalf("no request id in %q", text)
	}
	rest := text[start+len(`"id":"`):]
	end := strings.IndexByte(rest, '"')
	return rest[:end]
}

func TestNotificationDeliveryAndParamsNormalization(t *testing.T) {
	client, server := wirePair(t)
	seen := make(chan [2]string, 4)
	server.OnNotification(func(method string, params map[string]any) {
		encoded, _ := json.Marshal(params)
		seen <- [2]string{method, string(encoded)}
	})
	client.Notify("session.status", map[string]any{"sessionId": "s-9", "status": "running"})
	// Array and scalar params collapse to an empty object.
	client.Notify("ping", []any{1})
	client.Notify("pong", nil)
	got := [3]string{}
	for index := range got {
		select {
		case pair := <-seen:
			switch pair[0] {
			case "session.status":
				got[0] = pair[1]
			case "ping":
				if pair[1] != "{}" {
					t.Fatalf("ping params = %s", pair[1])
				}
				got[1] = "ok"
			case "pong":
				if pair[1] != "{}" {
					t.Fatalf("pong params = %s", pair[1])
				}
				got[2] = "ok"
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("notification %d never arrived", index)
		}
	}
	if !strings.Contains(got[0], `"status":"running"`) || got[1] != "ok" || got[2] != "ok" {
		t.Fatalf("notifications = %#v", got)
	}
}

func TestNotifyWithoutParamsOmitsMember(t *testing.T) {
	out := &captureWriter{}
	transport := NewLineTransport(strings.NewReader(""), out)
	transport.Notify("ping", nil)
	if got := out.String(); got != "{\"jsonrpc\":\"2.0\",\"method\":\"ping\"}\n" {
		t.Fatalf("frame = %q", got)
	}
	transport.Notify("ping", map[string]any{"a": 1.0})
	if got := out.String(); !strings.HasSuffix(got, ",\"params\":{\"a\":1}}\n") {
		t.Fatalf("frame = %q", got)
	}
}

// captureWriter records every written byte.
type captureWriter struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (w *captureWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *captureWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func waitFor(t *testing.T, out *captureWriter, needle string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), needle) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timeout waiting for %q in %q", needle, out.String())
}

func TestMalformedLinesIgnored(t *testing.T) {
	// Garbage lines of every non-frame shape precede one notification; only
	// the notification lands.
	out := &captureWriter{}
	in, inWriter := io.Pipe()
	transport := NewLineTransport(in, out)
	notes0 := make(chan [2]string, 4)
	transport.OnNotification(func(method string, params map[string]any) {
		encoded, _ := json.Marshal(params)
		notes0 <- [2]string{method, string(encoded)}
	})
	transport.Start()
	for _, line := range []string{
		"not json at all",
		"[]",
		"42",
		// A non-string non-number id makes the frame neither request nor
		// response, and the official parser falls through to the
		// notification path — so only the method line below answers.
		`{"id":true,"method":"nope"}`,
		`{"method":"note.a","params":{"x":1}}`,
	} {
		if _, err := inWriter.Write([]byte(line + "\n")); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	notes := [][2]string{}
	deadline := time.Now().Add(2 * time.Second)
	for len(notes) < 2 && time.Now().Before(deadline) {
		select {
		case note := <-notes0:
			notes = append(notes, note)
		case <-time.After(deadline.Sub(time.Now())):
		}
	}
	if len(notes) != 2 || notes[0][0] != "nope" || notes[1][0] != "note.a" {
		t.Fatalf("notes = %#v", notes)
	}
}

func TestStringAndNumberIDsDoNotCollide(t *testing.T) {
	// id "7" and id 7 are distinct pending keys: a numeric response must not
	// settle a string-id request.
	out := &captureWriter{}
	in, inWriter := io.Pipe()
	transport := NewLineTransport(in, out)
	transport.OnRequest(func(method string, params map[string]any) (any, error) {
		return map[string]any{"served": method}, nil
	})
	transport.Start()
	reply := make(chan error, 1)
	type settled struct {
		result any
		err    error
	}
	results := make(chan settled, 1)
	go func() {
		result, err := transport.Request(context.Background(), "probe", nil)
		results <- settled{result: result, err: err}
	}()
	waitFor(t, out, `"method":"probe"`)
	id := lastRequestID(t, out)
	if !strings.HasPrefix(id, "req_") {
		t.Fatalf("id = %q", id)
	}
	if _, err := inWriter.Write([]byte(`{"jsonrpc":"2.0","id":7,"result":{"wrong":true}}` + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	// The numeric response matched nothing; answering the real string id
	// settles the request with the right result.
	if _, err := inWriter.Write([]byte(`{"jsonrpc":"2.0","id":"` + id + `","result":{"ok":true}}` + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case done := <-results:
		if done.err != nil {
			t.Fatalf("err = %v", done.err)
		}
		result := done.result.(map[string]any)
		if result["ok"] != true {
			t.Fatalf("result = %#v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request never settled")
	}
	_ = reply
}

func TestRequestContextAbandonment(t *testing.T) {
	in, inWriter := io.Pipe()
	out := &captureWriter{}
	transport := NewLineTransport(in, out)
	transport.Start()
	ctx, cancel := context.WithCancel(context.Background())
	results := make(chan settled, 1)
	go func() {
		result, err := transport.Request(ctx, "slow", nil)
		results <- settled{result: result, err: err}
	}()
	waitFor(t, out, `"method":"slow"`)
	cancel()
	select {
	case done := <-results:
		if !errors.Is(done.err, context.Canceled) {
			t.Fatalf("err = %v", done.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("abandoned request never failed")
	}
	// The pending entry is gone: a late response settles nothing and does
	// not panic.
	if _, err := inWriter.Write([]byte(`{"jsonrpc":"2.0","id":"req_stale","result":{}}` + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
}

type settled struct {
	result any
	err    error
}

func TestCloseFailsPending(t *testing.T) {
	out := &captureWriter{}
	in := &blockingReader{}
	transport := NewLineTransport(in, out)
	transport.Start()
	results := make(chan settled, 1)
	go func() {
		result, err := transport.Request(context.Background(), "shutdown", nil)
		results <- settled{result: result, err: err}
	}()
	waitFor(t, out, `"method":"shutdown"`)
	transport.Close()
	select {
	case done := <-results:
		if done.err == nil || done.err.Error() != "JSON-RPC transport closed" {
			t.Fatalf("err = %v", done.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request did not fail after Close")
	}
}

// blockingReader parks reads forever, like a live stdio peer that never
// writes.
type blockingReader struct{}

func (*blockingReader) Read([]byte) (int, error) {
	time.Sleep(time.Hour)
	return 0, io.EOF
}

func TestInputEndFailsPending(t *testing.T) {
	out := &captureWriter{}
	in, inWriter := io.Pipe()
	transport := NewLineTransport(in, out)
	transport.Start()
	results := make(chan settled, 1)
	go func() {
		result, err := transport.Request(context.Background(), "shutdown", nil)
		results <- settled{result: result, err: err}
	}()
	waitFor(t, out, `"method":"shutdown"`)
	// End the input: buffered pending requests fail with the closed error.
	if err := inWriter.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case done := <-results:
		if done.err == nil || done.err.Error() != "JSON-RPC input closed" {
			t.Fatalf("err = %v", done.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending request not failed on input end")
	}
}

func TestIDKeySeparatesShapes(t *testing.T) {
	if idKey("7") == idKey(7.0) {
		t.Fatal("string and number ids share a key")
	}
	if idKey("7") != "s:7" || idKey(7.0) != "n:7" {
		t.Fatalf("keys = %q %q", idKey("7"), idKey(7.0))
	}
}

func TestDecodeIDShapes(t *testing.T) {
	if _, ok := decodeID(json.RawMessage(`"abc"`)); !ok {
		t.Fatal("string id rejected")
	}
	if _, ok := decodeID(json.RawMessage(`12`)); !ok {
		t.Fatal("number id rejected")
	}
	if _, ok := decodeID(json.RawMessage(`null`)); ok {
		t.Fatal("null accepted as id")
	}
	if _, ok := decodeID(json.RawMessage(`{"a":1}`)); ok {
		t.Fatal("object accepted as id")
	}
}

func TestWireTypesJSON(t *testing.T) {
	result := InitializeResult{ServerInfo: ServerIdentity{Name: ServerName, Version: "1"}}
	if result.ServerInfo.Name != "deepseek-harness-sdk-runtime" {
		t.Fatal("server name drifted")
	}
	want := InitializeParams{Cwd: `D:\w`, Provider: "deepseek", Model: "m", ReasoningEffort: "high", MaxTokens: 64}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back InitializeParams
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(back, want) {
		t.Fatalf("roundtrip = %+v", back)
	}
	finished := SubagentFinishedNotification{Status: RunStatusOk, StopReason: "completed"}
	if finished.Status != "ok" {
		t.Fatal("run status drifted")
	}
	if !ImageMimeTypes["image/png"] || ImageMimeTypes["image/svg+xml"] {
		t.Fatal("mime allowlist drifted")
	}
	_ = SessionPromptParams{SessionID: "s", ContentBlocks: []json.RawMessage{json.RawMessage(`{"type":"text"}`)}}
}

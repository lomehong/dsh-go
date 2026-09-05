// The $events/result unary endpoint: the browser answer POST routes into
// the pending waterfall waiter (the answer loop's host half).
package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dshgo/cordis"
	"dshgo/gatewaystream"
	"dshgo/typert"
)

func resultHandler(t *testing.T) *Gateway {
	t.Helper()
	root := cordis.NewRoot(cordis.Discard{})
	return New(root, typert.NewRegistry(root, cordis.Discard{}))
}

func postResult(t *testing.T, handler http.HandlerFunc, body map[string]any) (int, map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"type": "client-request", "rpcId": "rpc-1", "method": "$events/result", "payload": map[string]any{"args": body},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/$events/result", bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler(recorder, request)
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("response = %q", recorder.Body.String())
	}
	return recorder.Code, response
}

func TestEventResultDeliversToPendingWaiter(t *testing.T) {
	gateway := resultHandler(t)
	wait := gateway.Results().Await("evt-1")
	defer gateway.Results().Forget("evt-1")

	code, response := postResult(t, gateway.UnaryHandler(), map[string]any{
		"clientId": "client-1", "eventId": "evt-1",
		"outcome": map[string]any{"kind": "result", "value": "allowed-once"},
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	result := response["result"].(map[string]any)
	if result["ok"] != true {
		t.Fatalf("response = %#v", response)
	}
	select {
	case delivered := <-wait:
		if delivered.Outcome.Kind != "result" || delivered.Outcome.Value != "allowed-once" {
			t.Fatalf("delivered = %#v", delivered)
		}
	default:
		t.Fatal("no outcome routed to the waiter")
	}
}

func TestEventResultAnswersNotFoundForUnknownEvent(t *testing.T) {
	gateway := resultHandler(t)
	code, response := postResult(t, gateway.UnaryHandler(), map[string]any{
		"clientId": "client-1", "eventId": "evt-missing",
		"outcome": map[string]any{"kind": "next"},
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	result := response["result"].(map[string]any)
	if result["ok"] != false {
		t.Fatalf("response = %#v", response)
	}
}

func TestEventResultRejectsMalformedPayload(t *testing.T) {
	gateway := resultHandler(t)
	code, response := postResult(t, gateway.UnaryHandler(), map[string]any{
		"eventId": "evt-1",
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	result := response["result"].(map[string]any)
	if result["ok"] != false {
		t.Fatalf("response = %#v", response)
	}
}

// The router drops a late result for an already-settled event.
func TestResultRouterForgetsSettledEvents(t *testing.T) {
	router := gatewaystream.NewResultRouter()
	wait := router.Await("evt-9")
	if !router.Deliver(gatewaystream.RemoteEventResult{EventID: "evt-9", Outcome: gatewaystream.RemoteEventOutcome{Kind: "next"}}) {
		t.Fatal("first delivery must route")
	}
	delivered := <-wait
	if delivered.Outcome.Kind != "next" {
		t.Fatal("outcome mismatch")
	}
	if router.Deliver(gatewaystream.RemoteEventResult{EventID: "evt-9", Outcome: gatewaystream.RemoteEventOutcome{Kind: "next"}}) {
		t.Fatal("a settled event must not route twice")
	}
}

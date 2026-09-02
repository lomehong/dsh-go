package gatewaystream

import (
	"testing"
)

func TestParseClientMessageValidates(t *testing.T) {
	msg, err := parseClientMessage([]byte(`{"type":"open","streamId":"s1","endpoint":"a/b","payload":{"x":1}}`))
	if err != nil || msg.Kind != "open" || msg.Endpoint != "a/b" || msg.StreamID != "s1" {
		t.Fatalf("open = %+v %v", msg, err)
	}
	msg, err = parseClientMessage([]byte(`{"type":"open","streamId":"s1","endpoint":"a/b","payload":3}`))
	if err != nil || msg.Payload != float64(3) {
		t.Fatalf("scalar payload = %+v %v", msg, err)
	}
	msg, err = parseClientMessage([]byte(`{"type":"cancel","streamId":"s1"}`))
	if err != nil || msg.Kind != "cancel" {
		t.Fatalf("cancel = %+v %v", msg, err)
	}
	bad := []string{
		`not-json`,
		`{"type":"mystery"}`,
		`{"type":"open"}`,
		`{"type":"open","endpoint":""}`,
	}
	for _, raw := range bad {
		if _, err := parseClientMessage([]byte(raw)); err == nil {
			t.Fatalf("invalid accepted: %s", raw)
		}
	}
}

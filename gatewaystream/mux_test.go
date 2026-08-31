package gatewaystream

import (
	"testing"
)

func TestParseClientMessageValidates(t *testing.T) {
	msg, err := parseClientMessage([]byte(`{"kind":"open","endpoint":"a/b","payload":{"x":1}}`))
	if err != nil || msg.Kind != "open" || msg.Endpoint != "a/b" {
		t.Fatalf("open = %+v %v", msg, err)
	}
	msg, err = parseClientMessage([]byte(`{"kind":"open","endpoint":"a/b","payload":3}`))
	if err != nil || msg.Payload != float64(3) {
		t.Fatalf("scalar payload = %+v %v", msg, err)
	}
	msg, err = parseClientMessage([]byte(`{"kind":"close"}`))
	if err != nil || msg.Kind != "close" {
		t.Fatalf("close = %+v %v", msg, err)
	}
	bad := []string{
		`not-json`,
		`{"kind":"mystery"}`,
		`{"kind":"open"}`,
		`{"kind":"open","endpoint":""}`,
	}
	for _, raw := range bad {
		if _, err := parseClientMessage([]byte(raw)); err == nil {
			t.Fatalf("invalid accepted: %s", raw)
		}
	}
}

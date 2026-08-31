package gatewaystream

import (
	"strings"
	"testing"
)

func TestParseRemoteEventResultValidates(t *testing.T) {
	good := map[string]any{
		"clientId": "c1",
		"eventId":  "e1",
		"outcome":  map[string]any{"kind": "result", "value": map[string]any{"x": float64(1)}},
	}
	result, err := ParseRemoteEventResult(good)
	if err != nil || result.Outcome.Kind != "result" {
		t.Fatalf("good result = %+v %v", result, err)
	}
	next := map[string]any{"clientId": "c1", "eventId": "e2", "outcome": map[string]any{"kind": "next"}}
	result, err = ParseRemoteEventResult(next)
	if err != nil || result.Outcome.Kind != "next" {
		t.Fatalf("next = %+v %v", result, err)
	}
	rejected := map[string]any{
		"clientId": "c1", "eventId": "e3",
		"outcome": map[string]any{"kind": "rejected", "error": map[string]any{"name": "N", "message": "m", "code": "x"}},
	}
	result, err = ParseRemoteEventResult(rejected)
	if err != nil || result.Outcome.Kind != "rejected" || result.Outcome.Error == nil || result.Outcome.Error.Code != "x" {
		t.Fatalf("rejected = %+v %v", result, err)
	}
	cases := []map[string]any{
		{"clientId": "", "eventId": "e", "outcome": map[string]any{"kind": "next"}},
		{"clientId": "c", "eventId": "e", "outcome": map[string]any{"kind": "mystery"}},
		{"clientId": "c", "eventId": "e", "outcome": map[string]any{"kind": "result", "value": func() {}}},
		{"clientId": "c", "eventId": "e", "outcome": map[string]any{"kind": "rejected", "error": map[string]any{"name": "N"}}},
	}
	for i, tc := range cases {
		if _, err := ParseRemoteEventResult(tc); err == nil {
			t.Fatalf("case %d: invalid result accepted", i)
		}
	}
}

func TestProjectRestoreRejection(t *testing.T) {
	projected := ProjectRemoteEventRejection(map[string]any{"name": "N", "message": "m", "code": "c", "details": map[string]any{"k": float64(1)}})
	if projected.Name != "N" || projected.Message != "m" || projected.Code != "c" {
		t.Fatalf("projected = %+v", projected)
	}
	restored := RestoreRemoteEventRejection(projected)
	if restored.Error() != "m" {
		t.Fatalf("restored = %v", restored)
	}
	if named, ok := restored.(interface{ Name() string }); !ok || named.Name() != "N" {
		t.Fatalf("restored name lost")
	}
	plain := ProjectRemoteEventRejection("boom")
	if !strings.Contains(plain.Message, "boom") {
		t.Fatalf("plain = %+v", plain)
	}
}

func TestIsRemoteJSONValue(t *testing.T) {
	good := []any{nil, true, "x", float64(1), []any{float64(1), "a"}, map[string]any{"k": []any{}}}
	for _, value := range good {
		if !isRemoteJSONValue(value) {
			t.Fatalf("good value rejected: %#v", value)
		}
	}
	bad := []any{struct{ X int }{1}, func() {}, make(chan int)}
	for _, value := range bad {
		if isRemoteJSONValue(value) {
			t.Fatalf("bad value accepted: %#v", value)
		}
	}
}

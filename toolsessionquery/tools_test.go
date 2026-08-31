package toolsessionquery

import (
	"strings"
	"testing"
)

func TestResolveConfigDefaultsAndGates(t *testing.T) {
	resolved, err := ResolveConfig(Config{})
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if resolved.MaxSearchResults != 100 || resolved.SearchTimeoutMs != 30000 {
		t.Fatalf("defaults = %+v", resolved)
	}
	zero := 0
	if _, err := ResolveConfig(Config{MaxSearchResults: &zero}); err == nil || !strings.Contains(err.Error(), "positive") {
		t.Fatalf("zero max = %v", err)
	}
	neg := -1
	if _, err := ResolveConfig(Config{SearchTimeoutMs: &neg}); err == nil || !strings.Contains(err.Error(), "positive") {
		t.Fatalf("neg timeout = %v", err)
	}
}

func TestPromptTextAndEmptyFormat(t *testing.T) {
	if !strings.Contains(PromptText, "session_search") || !strings.Contains(PromptText, "session_event_read") {
		t.Fatalf("prompt = %q", PromptText)
	}
	if empty := formatEmptySearch(); empty == "" {
		t.Fatal("empty search format must be non-empty")
	}
}

func TestDecodeArgsRoundTrips(t *testing.T) {
	var request EventTargetArguments
	if err := decodeArgs(map[string]any{"session_id": "s1", "seq": float64(7)}, &request); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if request.SessionID != "s1" || request.Seq != 7 {
		t.Fatalf("request = %+v", request)
	}
}

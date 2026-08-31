package apiremotes

import "testing"

func TestForwardedEventsAllowlist(t *testing.T) {
	if len(ForwardedEvents) != 18 {
		t.Fatalf("allowlist = %d entries, want 18", len(ForwardedEvents))
	}
	seen := map[string]bool{}
	for _, entry := range ForwardedEvents {
		if entry.Event == "" || (entry.Mode != "emit" && entry.Mode != "waterfall") {
			t.Fatalf("invalid entry: %+v", entry)
		}
		if seen[entry.Event] {
			t.Fatalf("duplicate event: %s", entry.Event)
		}
		seen[entry.Event] = true
	}
}

func TestIsForwardedAndModeOf(t *testing.T) {
	if !IsForwarded("commands/change") {
		t.Fatal("commands/change must be forwarded")
	}
	if ModeOf("commands/change") != "emit" {
		t.Fatalf("commands/change mode = %q", ModeOf("commands/change"))
	}
	if ModeOf("approval/request") != "waterfall" {
		t.Fatalf("approval/request mode = %q", ModeOf("approval/request"))
	}
	if IsForwarded("brand/not-forwarded") {
		t.Fatal("unlisted event must not be forwarded")
	}
	if ModeOf("brand/not-forwarded") != "" {
		t.Fatalf("unlisted mode = %q", ModeOf("brand/not-forwarded"))
	}
	// The reference-change entry resolves to the allowlisted wire name.
	if !IsForwarded(refChanged()) {
		t.Fatal("reference-change event must be forwarded")
	}
}

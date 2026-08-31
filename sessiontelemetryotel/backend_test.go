package sessiontelemetryotel

import (
	"strings"
	"testing"

	"dshgo/sessiontelemetry"
)

func TestNewDisabledBackendNoSDK(t *testing.T) {
	backend, err := New(Config{Mode: ModeDisabled})
	if err != nil {
		t.Fatalf("disabled: %v", err)
	}
	if backend.Sharing() != "disabled" {
		t.Fatalf("sharing = %q", backend.Sharing())
	}
	// DISABLED emits nothing and shutdown is a no-op.
	backend.Emit(sessiontelemetry.Record{Channel: "ledger"})
	if err := backend.Shutdown(); err != nil {
		t.Fatalf("disabled shutdown: %v", err)
	}
}

func TestNewRequiresURLOutsideDisabled(t *testing.T) {
	_, err := New(Config{Mode: ModeFull})
	if err == nil || !strings.Contains(err.Error(), "exporter.url is required") {
		t.Fatalf("missing url = %v", err)
	}
}

func TestNewRejectsBadURLAndScheme(t *testing.T) {
	// Go's url.Parse is lenient for bare words (they become a path with an
	// empty scheme, landing on the http(s) check); a genuinely malformed
	// URL triggers the parse error.
	if _, err := New(Config{Mode: ModeFull, URL: "http://exa mple.com/v1/logs"}); err == nil || !strings.Contains(err.Error(), "not a valid URL") {
		t.Fatalf("bad url = %v", err)
	}
	if _, err := New(Config{Mode: ModeFull, URL: "ftp://example.com/v1/logs"}); err == nil || !strings.Contains(err.Error(), "must be http(s)") {
		t.Fatalf("bad scheme = %v", err)
	}
	if _, err := New(Config{Mode: ModeFull, URL: "not a url"}); err == nil || !strings.Contains(err.Error(), "must be http(s)") {
		t.Fatalf("bare word url = %v", err)
	}
}

func TestNewRejectsBadBatchAndTimeout(t *testing.T) {
	zero := 0
	if _, err := New(Config{Mode: ModeFull, URL: "http://collector:4318/v1/logs", MaxExportBatchSize: &zero}); err == nil || !strings.Contains(err.Error(), "maxExportBatchSize") {
		t.Fatalf("batch = %v", err)
	}
	neg := int64(-5)
	if _, err := New(Config{Mode: ModeFull, URL: "http://collector:4318/v1/logs", ShutdownTimeoutMillis: &neg}); err == nil || !strings.Contains(err.Error(), "shutdownTimeoutMillis") {
		t.Fatalf("timeout = %v", err)
	}
	zeroTimeout := int64(0)
	if _, err := New(Config{Mode: ModeFull, URL: "http://collector:4318/v1/logs", ShutdownTimeoutMillis: &zeroTimeout}); err == nil || !strings.Contains(err.Error(), "shutdownTimeoutMillis") {
		t.Fatalf("zero timeout = %v", err)
	}
}

func TestResolveModeRejectsUnknown(t *testing.T) {
	if _, err := resolveMode(Mode("SOMETHING")); err == nil || !strings.Contains(err.Error(), "unsupported mode") {
		t.Fatalf("unknown mode = %v", err)
	}
	if mode, err := resolveMode(""); err != nil || mode != ModeDisabled {
		t.Fatalf("empty mode = %q %v", mode, err)
	}
}

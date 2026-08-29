package sandboxpolicy

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultsAreFailSafeReadOnlyWithProcessCwd(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(Config{}, cwd)
	if err != nil {
		t.Fatalf("empty config: %v", err)
	}
	if service.DefaultMode() != "read-only" {
		t.Fatalf("default mode: %q", service.DefaultMode())
	}
	if !filepath.IsAbs(service.WorkspaceRoot()) {
		t.Fatalf("fallback root must be absolute: %q", service.WorkspaceRoot())
	}
	policy := service.Resolve("", "", "")
	if policy.Mode != "read-only" || policy.WorkspaceRoot != service.WorkspaceRoot() {
		t.Fatalf("resolve: %+v", policy)
	}
}

func TestExplicitConfigAndUnknownModeFailsLoud(t *testing.T) {
	if _, err := NewService(Config{Mode: "lax"}, ""); err == nil {
		t.Fatal("unknown mode must fail loud")
	}
	service, err := NewService(Config{Mode: "workspace-write", WorkspaceRoot: "relative/root"}, "")
	if err != nil {
		t.Fatalf("explicit config: %v", err)
	}
	if !filepath.IsAbs(service.WorkspaceRoot()) {
		t.Fatalf("configured root must be stored absolute: %q", service.WorkspaceRoot())
	}
}

func TestResolutionPrecedenceApprovedOverOverrideOverDefault(t *testing.T) {
	service, err := NewService(Config{Mode: "read-only", WorkspaceRoot: t.TempDir()}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := service.Resolve("", "", "").Mode; got != "read-only" {
		t.Fatalf("bare resolve: %q", got)
	}
	if got := service.Resolve("", "workspace-write", "").Mode; got != "workspace-write" {
		t.Fatalf("override outranks default: %q", got)
	}
	if got := service.Resolve("", "read-only", "danger-full-access").Mode; got != "danger-full-access" {
		t.Fatalf("approved outranks override: %q", got)
	}
	// A session cwd replaces the workspace boundary.
	sessionRoot := t.TempDir()
	if got := service.Resolve(sessionRoot, "", "").WorkspaceRoot; got != sessionRoot {
		t.Fatalf("session cwd boundary: %q", got)
	}
}

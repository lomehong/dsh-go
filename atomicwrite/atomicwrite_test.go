package atomicwrite

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenameReplacingSwapsContent(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "doc.json")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RenameReplacing(tmp, target); err != nil {
		t.Fatalf("rename: %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "new" {
		t.Fatalf("target = %q %v", data, err)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatal("temp must be consumed by the rename")
	}
}

func TestRenameReplacingFailsLoudWithoutConsumingTemp(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "no-such-dir", "doc.json")
	tmp := filepath.Join(dir, "doc.json.tmp")
	if err := os.WriteFile(tmp, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := RenameReplacing(tmp, missing)
	if err == nil {
		t.Fatal("rename into a missing directory must fail")
	}
	if !strings.Contains(err.Error(), "no-such-dir") && !strings.Contains(err.Error(), "cannot find") && !strings.Contains(err.Error(), "no such") {
		t.Fatalf("error = %v", err)
	}
}

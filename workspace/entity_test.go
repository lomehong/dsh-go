package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newEntityOnDirectory bootstraps one workspace over an existing directory
// with one accounted session.
func newEntityOnDirectory(t *testing.T, id string) (*Registry, string, *Entity, string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	persistence := &fakePersistence{}
	persistence.seed("s-1", dir, 10)
	registry, _ := newRegistry(t, RegistryHost{Persistence: persistence})
	created, err := registry.Create(context.Background(), dir, "Alpha")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return registry, root, created, dir
}

func TestSetTitleSameValueStillWrites(t *testing.T) {
	_, _, entity, _ := newEntityOnDirectory(t, "title-same")
	before := entity.UpdatedAt()
	time.Sleep(3 * time.Millisecond)
	// The official `setTitle` always builds a new record, so a same-value
	// replace still refreshes updatedAt (reference equality, not value
	// equality, decides the no-op).
	if err := entity.SetTitle("Alpha"); err != nil {
		t.Fatalf("same-value SetTitle: %v", err)
	}
	if !after(entity.UpdatedAt(), before) {
		t.Fatalf("same-value SetTitle must rewrite: %s stayed %s", before, entity.UpdatedAt())
	}
	// A changed value writes too.
	time.Sleep(3 * time.Millisecond)
	before = entity.UpdatedAt()
	if err := entity.SetTitle("Beta"); err != nil {
		t.Fatalf("SetTitle: %v", err)
	}
	if entity.Title() != "Beta" || !after(entity.UpdatedAt(), before) {
		t.Fatalf("SetTitle = %s at %s, want Beta written after %s", entity.Title(), entity.UpdatedAt(), before)
	}
}

func TestIdempotentMutationsStayNoOps(t *testing.T) {
	_, _, entity, _ := newEntityOnDirectory(t, "idempotent")
	if err := entity.AttachSession("s-1"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	before := entity.UpdatedAt()
	time.Sleep(3 * time.Millisecond)
	// Already-accounted attach, missing detach, and move-to-same-position
	// return the record verbatim: no rewrite, no updatedAt refresh.
	if err := entity.AttachSession("s-1"); err != nil {
		t.Fatalf("re-attach: %v", err)
	}
	if err := entity.DetachSession("s-absent"); err != nil {
		t.Fatalf("detach absent: %v", err)
	}
	if err := entity.InsertSessionBefore("s-1", ""); err != nil {
		t.Fatalf("move to same position: %v", err)
	}
	if err := entity.InsertSessionBefore("s-1", "s-1"); err != nil {
		t.Fatalf("self anchor: %v", err)
	}
	if entity.UpdatedAt() != before {
		t.Fatalf("idempotent mutations must not write: %s became %s", before, entity.UpdatedAt())
	}
	// An unaccounted anchor rejects without writing either.
	if err := entity.InsertSessionBefore("s-1", "s-absent"); err == nil {
		t.Fatal("anchor must be accounted")
	}
	if entity.UpdatedAt() != before {
		t.Fatalf("rejected move must not write: %s became %s", before, entity.UpdatedAt())
	}
}

func TestAttachUnresolvableCwdCarriesCause(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "attach-cause")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	persistence := &fakePersistence{}
	persistence.seed("s-1", dir, 10)
	persistence.seed("s-lost", filepath.Join(root, "vanished"), 20)
	registry, _ := newRegistry(t, RegistryHost{Persistence: persistence})
	created, err := registry.Create(context.Background(), dir, "Alpha")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	err = created.AttachSession("s-lost")
	if err == nil {
		t.Fatal("an unresolvable cwd must reject")
	}
	// The official error carries the resolve failure as {cause}: the Go
	// wrapper keeps the message verbatim and preserves the chain.
	if errors.Unwrap(err) == nil {
		t.Fatalf("the resolve failure must stay unwrappable, got %v", err)
	}
	if !strings.Contains(err.Error(), "does not resolve") {
		t.Fatalf("error = %v, want the verbatim message over a cause", err)
	}
}

// after reports whether an ISO-8601 millisecond timestamp strictly follows
// another.
func after(candidate string, before string) bool {
	c, err1 := time.Parse("2006-01-02T15:04:05.000Z07:00", candidate)
	b, err2 := time.Parse("2006-01-02T15:04:05.000Z07:00", before)
	if err1 != nil || err2 != nil {
		return false
	}
	return c.After(b)
}

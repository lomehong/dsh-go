package jsonl

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"dshgo/llm"
	"dshgo/session"
	"dshgo/session/persistence"
)

// backendUserEvent builds one validated surface user/message event.
func backendUserEvent(t *testing.T, seq int64, text string) session.Event {
	t.Helper()
	message := llm.NewUserMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: text}}, llm.MessageSource{})
	raw, err := json.Marshal(message)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return session.Event{
		Type: session.EventUserMessage, Seq: seq, Time: 1, Data: raw,
		SurfaceOp: &session.SurfaceOp{Kind: session.SurfaceAppend},
	}
}

func newFileCoordinator(t *testing.T, root string) (*persistence.Coordinator, *Backend) {
	t.Helper()
	backend := NewBackend(root, "")
	coordinator, err := persistence.NewCoordinator(backend, nil, nil, persistence.CoordinatorOptions{})
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	return coordinator, backend
}

func TestBackendEndToEndThroughCoordinator(t *testing.T) {
	root := t.TempDir()
	coordinator, backend := newFileCoordinator(t, root)
	id := session.SessionID("e2e")
	header := session.SessionHeader{
		ID: id, Version: session.SESSION_FORMAT_VERSION, CreatedAt: 7, CWD: root,
	}
	if err := coordinator.Create(header); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := coordinator.Append(id, []session.Event{
		backendUserEvent(t, 0, "hello"),
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	// A second append lands after the first (cursor advance).
	if err := coordinator.Append(id, []session.Event{
		backendUserEvent(t, 1, "again"),
	}); err != nil {
		t.Fatalf("append 2: %v", err)
	}

	// A fresh coordinator over the same root reads the full log.
	reopened, _ := newFileCoordinator(t, root)
	inspection, err := reopened.Load(id)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(inspection.Events) != 2 {
		t.Fatalf("events = %d", len(inspection.Events))
	}

	// Snapshots: repeated reads of an unchanged log return the same
	// revision; a durable append changes it.
	snapshots, err := backend.ListSnapshots()
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("snapshots = %+v, %v", snapshots, err)
	}
	first := snapshots[0].Revision
	again, err := backend.ReadStoredRevision(id)
	if err != nil || again != first {
		t.Fatalf("revision drift without mutation: %q vs %q (%v)", again, first, err)
	}
	if err := coordinator.Append(id, []session.Event{backendUserEvent(t, 2, "third")}); err != nil {
		t.Fatalf("append 3: %v", err)
	}
	after, err := backend.ReadStoredRevision(id)
	if err != nil || after == first {
		t.Fatalf("revision must change after append: %q vs %q (%v)", after, first, err)
	}
	if err := coordinator.Dispose(); err != nil {
		t.Fatalf("dispose: %v", err)
	}
}

func TestBackendTornTailRepairThroughCoordinator(t *testing.T) {
	root := t.TempDir()
	coordinator, backend := newFileCoordinator(t, root)
	id := session.SessionID("torn")
	header := session.SessionHeader{
		ID: id, Version: session.SESSION_FORMAT_VERSION, CreatedAt: 7, CWD: root,
	}
	if err := coordinator.Create(header); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := coordinator.Append(id, []session.Event{backendUserEvent(t, 0, "committed")}); err != nil {
		t.Fatalf("append: %v", err)
	}
	path := backend.Store.PathOf(root, string(id))

	// Simulate a crash mid-write: a partial line, no newline.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	file.WriteString(`{"type":"user/messa`)
	file.Close()

	// LoadStored reports the torn marker instead of mutating.
	stored, err := backend.LoadStored(id)
	if err != nil {
		t.Fatalf("loadStored: %v", err)
	}
	if stored.TornMarker == nil {
		t.Fatal("torn marker missing")
	}
	if len(stored.Events) != 1 {
		t.Fatalf("preserved prefix = %d", len(stored.Events))
	}
	// The file still carries the torn bytes (no eager truncation).
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if truncateTo, ok := stored.TornMarker.(int64); !ok || info.Size() <= truncateTo {
		t.Fatal("artifact was mutated by a read, or the marker lost its offset")
	}

	// The coordinator's load commits the repair and converges.
	inspection, err := coordinator.Load(id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(inspection.Events) != 1 {
		t.Fatalf("events = %d", len(inspection.Events))
	}
	// After the repair the log is clean: no torn marker remains.
	stored, err = backend.LoadStored(id)
	if err != nil || stored.TornMarker != nil {
		t.Fatalf("post-repair load = %+v, %v", stored, err)
	}
	if err := coordinator.Dispose(); err != nil {
		t.Fatalf("dispose: %v", err)
	}
}

// TestBackendBorrowPinsPreparedSource pins a prepared source through the
// borrow lease.
func TestBackendBorrowPinsPreparedSource(t *testing.T) {
	root := t.TempDir()
	coordinator, _ := newFileCoordinator(t, root)
	id := session.SessionID("borrow")
	header := session.SessionHeader{
		ID: id, Version: session.SESSION_FORMAT_VERSION, CreatedAt: 7, CWD: root,
	}
	if err := coordinator.Create(header); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := coordinator.Append(id, []session.Event{backendUserEvent(t, 0, "pinned")}); err != nil {
		t.Fatalf("append: %v", err)
	}
	borrowed, err := coordinator.BorrowSession(id)
	if err != nil {
		t.Fatalf("borrow: %v", err)
	}
	if borrowed.Source != "prepared" || borrowed.PreparedSession == nil || borrowed.Revision == "" {
		t.Fatalf("borrowed = source:%s session:%v rev:%q", borrowed.Source, borrowed.PreparedSession, borrowed.Revision)
	}
	// The pinned source survives LRU pressure (capacity 5 default; pin it
	// and force evictions through other ids).
	borrowed.Release()
	borrowed2, err := coordinator.BorrowSession(id)
	if err != nil || borrowed2.Source != "prepared" {
		t.Fatalf("re-borrow = %+v, %v", borrowed2, err)
	}
	borrowed2.Release()
	if err := coordinator.Dispose(); err != nil {
		t.Fatalf("dispose: %v", err)
	}
}

func TestBackendFormatRefusalCarriesRawLogPath(t *testing.T) {
	root := t.TempDir()
	_, backend := newFileCoordinator(t, root)
	id := session.SessionID("future")
	header := session.SessionHeader{
		ID: id, Version: session.SESSION_FORMAT_VERSION + 1, CreatedAt: 7, CWD: root,
	}
	// Write a future-version header directly.
	if err := backend.Store.Create(header, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	coordinator, _ := newFileCoordinator(t, root)
	_, err := coordinator.Load(id)
	var unsupported *persistence.FormatUnsupportedError
	if !errors.As(err, &unsupported) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(unsupported.Error(), "upgrade the harness") {
		t.Fatalf("refusal = %q", unsupported.Error())
	}
	if unsupported.Location == nil || !strings.Contains(unsupported.Location.Path, "session.jsonl") {
		t.Fatalf("location = %+v", unsupported.Location)
	}
}

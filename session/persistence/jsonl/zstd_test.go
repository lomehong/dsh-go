// The zstd frame container: round-trip create/append/load under the zstd
// compression, the torn-final-frame repair contract, and structural scan
// validity of the artifacts.
package jsonl

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dshgo/llm"
	"dshgo/session"
)

func zstdBackend(t *testing.T) *Backend {
	t.Helper()
	return NewBackend(t.TempDir(), CompressionZstd)
}

func mustJSONZstd(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return encoded
}

func userMessageForZstdTest(text string) llm.Message {
	return llm.NewUserMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: text}},
		llm.MessageSource{Kind: llm.SourceUser})
}

// turnBatch builds one completed turn: turn/start, one user message per
// text, turn/end — seqs continuing from start.
func turnBatch(t *testing.T, startSeq int64, turn int64, texts ...string) []session.Event {
	t.Helper()
	events := []session.Event{{
		Type: session.EventTurnStart, Seq: startSeq, Time: 100 + startSeq,
		Data: json.RawMessage(`{"turn":1}`),
	}}
	seq := startSeq + 1
	for _, text := range texts {
		message := userMessageForZstdTest(text)
		raw := mustJSONZstd(t, map[string]any{"message": message})
		events = append(events, session.Event{
			Type:      session.EventUserMessage,
			Seq:       seq,
			Time:      100 + seq,
			Data:      raw,
			SurfaceOp: &session.SurfaceOp{Kind: session.SurfaceAppend},
		})
		seq++
	}
	events = append(events, session.Event{
		Type: session.EventTurnEnd, Seq: seq, Time: 100 + seq,
		Data: json.RawMessage(`{"turn":1,"reason":{"kind":"completed"}}`),
	})
	return events
}

// The full persistence cycle under zstd: create (header+seed as one frame),
// append (one frame per batch), load (decode + scan), list (decoded header).
func TestZstdRoundTripCreateAppendLoadList(t *testing.T) {
	backend := zstdBackend(t)
	header := session.SessionHeader{
		Version: session.SESSION_FORMAT_VERSION,
		ID:      "session-zstd-1",
		CWD:     `D:\work`,
	}
	if err := backend.Store.Create(header, turnBatch(t, 0, 1, "seed one")); err != nil {
		t.Fatalf("create: %v", err)
	}
	path := backend.Store.PathOf(`D:\work`, "session-zstd-1")
	if !strings.HasSuffix(filepath.ToSlash(path), ".jsonl.zstd") {
		t.Fatalf("artifact suffix = %q", path)
	}

	if err := backend.AppendBatch(header, turnBatch(t, 3, 2, "batch two"), true); err != nil {
		t.Fatalf("append: %v", err)
	}

	scan, err := backend.Store.Load(`D:\work`, "session-zstd-1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if scan.TornTail {
		t.Fatalf("torn tail on a clean container")
	}
	if len(scan.Events) != 6 {
		t.Fatalf("events = %d, want both batches decoded", len(scan.Events))
	}

	headers, err := backend.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, entry := range headers {
		if entry.ID == header.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("list = %+v, want the zstd session", headers)
	}

	// The artifact bytes are a real Zstandard container: the container is
	// NOT plaintext, and its frames scan structurally.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if raw[0] == '{' {
		t.Fatal("artifact is plaintext under zstd compression")
	}
	scanResult, err := scanZstdFrames(raw)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(scanResult.frames) != 2 {
		t.Fatalf("frames = %d, want one per durable batch", len(scanResult.frames))
	}
	if scanResult.torn {
		t.Fatal("clean container must not report a torn frame")
	}
}

// A torn final frame (crash mid-flush) reports the repair boundary and the
// coordinator's CommitRepair truncates at the last complete frame.
func TestZstdTornFrameRepair(t *testing.T) {
	backend := zstdBackend(t)
	header := session.SessionHeader{
		Version: session.SESSION_FORMAT_VERSION,
		ID:      "session-zstd-2",
		CWD:     `D:\work`,
	}
	if err := backend.Store.Create(header, turnBatch(t, 0, 1, "safe batch")); err != nil {
		t.Fatalf("create: %v", err)
	}
	path := backend.Store.PathOf(`D:\work`, "session-zstd-2")

	// Simulate a crash mid-flush: append a partial frame (half of a real
	// frame's bytes) after the complete prefix.
	frame, err := compressZstdFrame([]byte("{\"type\":\"user/message\",\"data\":{}}\n"))
	if err != nil {
		t.Fatalf("compress: %v", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := file.Write(frame[:len(frame)/2]); err != nil {
		t.Fatalf("write torn: %v", err)
	}
	file.Close()

	stored, err := backend.LoadStored(header.ID)
	if err != nil {
		t.Fatalf("load stored: %v", err)
	}
	if stored.TornMarker == nil {
		t.Fatal("torn marker missing on a torn container")
	}
	truncateTo, ok := stored.TornMarker.(int64)
	if !ok || truncateTo <= 0 {
		t.Fatalf("torn marker = %v", stored.TornMarker)
	}
	if len(stored.Events) != 3 {
		t.Fatalf("events = %d, want the complete turn only", len(stored.Events))
	}

	// The coordinator's repair truncates at the frame boundary; the
	// write-behind layer re-flushes the lost batch afterwards.
	if err := backend.CommitRepair(header, stored.TornMarker, nil); err != nil {
		t.Fatalf("commit repair: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != truncateTo {
		t.Fatalf("size after repair = %d, want %d", info.Size(), truncateTo)
	}
	scan, err := backend.Store.Load(`D:\work`, "session-zstd-2")
	if err != nil {
		t.Fatalf("load after repair: %v", err)
	}
	if len(scan.Events) != 3 {
		t.Fatalf("events after repair = %d, want the complete turn", len(scan.Events))
	}
	// The repaired container accepts further appends.
	if err := backend.AppendBatch(header, turnBatch(t, 3, 2, "after repair"), true); err != nil {
		t.Fatalf("append after repair: %v", err)
	}
	scan, err = backend.Store.Load(`D:\work`, "session-zstd-2")
	if err != nil {
		t.Fatalf("load after re-append: %v", err)
	}
	if len(scan.Events) != 6 {
		t.Fatalf("events after re-append = %d", len(scan.Events))
	}
}

// The scan rejects a container whose first bytes are not a Zstandard frame.
func TestScanRejectsNonZstdContainer(t *testing.T) {
	if _, err := scanZstdFrames([]byte("{\"type\":\"session\"}\n")); err == nil {
		t.Fatal("plaintext container accepted by the zstd scan")
	}
}

// context keeps the context import meaningful for backend seam parity.
var _ = context.Background

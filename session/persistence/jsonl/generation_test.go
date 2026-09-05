package jsonl

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dshgo/session"
	"dshgo/session/persistence"
)

// legacyV0Log builds the bytes of a historical Go-written log: version-0
// header with seedLength, released-v1 logical events (top-level chunks).
func legacyV0Log(t *testing.T, id string, cwd string) ([]byte, session.SessionHeader) {
	t.Helper()
	header := session.SessionHeader{
		Version:   0,
		ID:        session.SessionID(id),
		CreatedAt: 1725500000000,
		CWD:       cwd,
	}
	headerLine := map[string]any{
		"type": "session", "version": int64(0), "id": id,
		"createdAt": int64(1725500000000), "delegationDepth": int64(0),
		"cwd": cwd,
	}
	headerJSON, err := json.Marshal(headerLine)
	if err != nil {
		t.Fatal(err)
	}
	events := []string{
		`{"type":"turn/start","seq":0,"time":1,"data":{"turn":1}}`,
		`{"type":"step/start","seq":1,"time":2,"data":{"turn":1,"step":1}}`,
		`{"type":"user/message","seq":2,"time":3,"data":{"id":"u1","role":"user","content":[{"type":"text","text":"hi"}],"source":{"kind":"user"}},"surfaceOp":"append"}`,
		`{"type":"assistant/chunk","seq":3,"time":4,"data":{"turn":1,"step":1,"chunk":{"type":"text-delta","index":0,"text":"he"}}}`,
		`{"type":"assistant/chunk","seq":4,"time":7,"data":{"turn":1,"step":1,"chunk":{"type":"text-delta","index":0,"text":"llo"}}}`,
		`{"type":"assistant/message","seq":5,"time":9,"data":{"turn":1,"step":1,"message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"hello"}],"source":{"kind":"model","provider":"p","model":"m"}}},"sourceEventSeqs":[3,4],"surfaceOp":"append"}`,
		`{"type":"step/end","seq":6,"time":10,"data":{"turn":1,"step":1}}`,
		`{"type":"turn/end","seq":7,"time":11,"data":{"turn":1,"reason":{"kind":"completed"}}}`,
	}
	buffer := append([]byte{}, headerJSON...)
	buffer = append(buffer, '\n')
	for _, line := range events {
		buffer = append(buffer, line...)
		buffer = append(buffer, '\n')
	}
	return buffer, header
}

func TestEnsureCurrentMigratesLegacyLog(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	payload, header := legacyV0Log(t, "legacy-1", cwd)
	st := &Store{Root: root}
	if err := os.MkdirAll(SessionDir(root, cwd, "legacy-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(SessionDir(root, cwd, "legacy-1"), "session.jsonl")
	if err := os.WriteFile(legacyPath, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	backend := NewBackend(root, CompressionNone)
	prefix, err := backend.LoadStored("legacy-1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if prefix.Meta.Version != session.SESSION_FORMAT_VERSION {
		t.Fatalf("migrated meta version: %d", prefix.Meta.Version)
	}

	// The successor exists; the source stays byte-identical.
	successor := filepath.Join(SessionDir(root, cwd, "legacy-1"), "session.v2.jsonl")
	if _, err := os.Stat(successor); err != nil {
		t.Fatalf("successor not published: %v", err)
	}
	sourceBytes, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(sourceBytes) != string(payload) {
		t.Fatal("the legacy source must remain byte-identical")
	}

	// The migrated body folds the chunks into the message settlement.
	chunks, messages, attempts := 0, 0, 0
	for _, event := range prefix.Events {
		switch event.Type {
		case "assistant/chunk":
			chunks++
		case "assistant/message":
			messages++
			if !strings.Contains(string(event.Data), `"stream":[{`) {
				t.Fatal("migrated message must embed the compact stream")
			}
		case "assistant/attempt":
			attempts++
		}
	}
	if chunks != 0 || messages != 1 || attempts != 0 {
		t.Fatalf("migrated shape: chunks=%d messages=%d attempts=%d", chunks, messages, attempts)
	}

	// A second load takes the current fast path (no republish) and appends
	// reach the successor.
	revision, err := backend.ReadStoredRevision("legacy-1")
	if err != nil || revision == "" {
		t.Fatalf("revision: %v %q", err, revision)
	}
	if err := backend.AppendBatch(header, []session.Event{}, true); err != nil {
		t.Fatalf("append after migration: %v", err)
	}
	scan, err := st.Load(cwd, "legacy-1")
	if err != nil {
		t.Fatalf("reload successor: %v", err)
	}
	// 8 v1 events with two consumed chunks fold into 6 dense v2 events.
	if len(scan.Events) != 6 {
		t.Fatalf("successor events: %d", len(scan.Events))
	}
	if scan.Meta.InheritedEventCount != 0 || scan.Meta.IsSeeded {
		t.Fatalf("unseeded meta: %+v", scan.Meta)
	}
	_ = persistence.StoredPrefix{}
}

func TestEnsureCurrentRefusesFutureGeneration(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	if err := os.MkdirAll(SessionDir(root, cwd, "future"), 0o755); err != nil {
		t.Fatal(err)
	}
	header := `{"type":"session","version":99,"id":"future","createdAt":1,"delegationDepth":0,"isSeeded":false}`
	path := filepath.Join(SessionDir(root, cwd, "future"), "session.v99.jsonl")
	if err := os.WriteFile(path, []byte(header+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	backend := NewBackend(root, CompressionNone)
	_, err := backend.LoadStored("future")
	var unsupported *persistence.FormatUnsupportedError
	if err == nil || !errorsAsFormatUnsupported(err, &unsupported) {
		t.Fatalf("future generation must refuse as unsupported, got %v", err)
	}
	if !strings.Contains(unsupported.Error(), "upgrade the harness") {
		t.Fatalf("refusal must direct the upgrade: %q", unsupported.Error())
	}
}

func TestCreateReservesIdAcrossGenerations(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	st := &Store{Root: root}
	if err := os.MkdirAll(SessionDir(root, cwd, "s1"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A v0-era log under the legacy name reserves the id even though the
	// current-generation path is absent.
	legacyPath := filepath.Join(SessionDir(root, cwd, "s1"), "session.jsonl")
	if err := os.WriteFile(legacyPath, []byte(`{"type":"session","version":0,"id":"s1","createdAt":1,"delegationDepth":0}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	header := session.SessionHeader{
		Version: session.SESSION_FORMAT_VERSION, ID: "s1", CreatedAt: 5, CWD: cwd,
	}
	if err := st.Create(header, nil); err == nil {
		t.Fatal("a legacy generation must reserve the session id")
	}
}

// errorsAsFormatUnsupported adapts errors.As for the persistence wrapper.
func errorsAsFormatUnsupported(err error, target **persistence.FormatUnsupportedError) bool {
	for err != nil {
		if typed, ok := err.(*persistence.FormatUnsupportedError); ok {
			*target = typed
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}

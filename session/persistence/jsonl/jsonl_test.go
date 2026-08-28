package jsonl

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dshgo/session"
)

func headerFor(id string) session.SessionHeader {
	depth := int64(0)
	return session.SessionHeader{
		Version: session.SESSION_FORMAT_VERSION, ID: session.SessionID(id),
		CreatedAt: 42, CWD: `D:\work\my project`, DelegationDepth: &depth,
	}
}

func turnStart(seq int64) session.Event {
	return session.Event{Type: session.EventTurnStart, Seq: seq, Time: 100 + seq,
		Data: json.RawMessage(`{"turn":1}`)}
}

func turnEnd(seq int64) session.Event {
	return session.Event{Type: session.EventTurnEnd, Seq: seq, Time: 100 + seq,
		Data: json.RawMessage(`{"turn":1,"reason":{"kind":"completed"}}`)}
}

func TestEncodeSegmentNeutralizesTraversal(t *testing.T) {
	if got, err := EncodeSegment("abc-123_x.y"); err != nil || got != "abc-123_x.y" {
		t.Fatalf("safe segment stays literal, got %q err %v", got, err)
	}
	if got, _ := EncodeSegment("."); got != "~002E" {
		t.Fatalf("dot escapes: %q", got)
	}
	if got, _ := EncodeSegment(".."); got != "~002E~002E" {
		t.Fatalf("dotdot escapes: %q", got)
	}
	if got, _ := EncodeSegment("a/b\\c:d~e"); got != "a~002Fb~005Cc~003Ad~007Ee" {
		t.Fatalf("separators escape: %q", got)
	}
	if got, _ := EncodeSegment("é"); got != "~00E9" {
		t.Fatalf("non-ASCII escapes: %q", got)
	}
	if _, err := EncodeSegment(""); err == nil {
		t.Fatal("empty segments are refused")
	}
	// The encoding is injective: distinct inputs never collide.
	a, _ := EncodeSegment("a~002F")
	b, _ := EncodeSegment("a/")
	if a == b {
		t.Fatal("the escape must not collide with literal text")
	}
}

func TestProjectKeySlugsProjectPaths(t *testing.T) {
	// Consecutive separators collapse into one dash; other unsafe units
	// (the space) escape.
	if got := ProjectKey(`D:\work\my project`); got != "--D-work-my~0020project--" {
		t.Fatalf("project key wrong: %q", got)
	}
	if got := ProjectKey("/home/user"); got != "--home-user--" {
		t.Fatalf("posix project key wrong: %q", got)
	}
	if got := ProjectKey(`\`); got != "--root--" {
		t.Fatalf("a separator-only path collapses to root, got %q", got)
	}
}

func TestCreateLoadAppendRoundTrip(t *testing.T) {
	root := t.TempDir()
	st := &Store{Root: root}
	header := headerFor("round-trip")

	seed := []session.Event{
		turnStart(0),
		{Type: session.EventUserMessage, Seq: 1, Time: 101,
			Data:      json.RawMessage(`{"message":{"id":"m1","role":"user","source":{"kind":"user"},"content":[{"type":"text","text":"hi"}]}}`),
			SurfaceOp: &session.SurfaceOp{Kind: session.SurfaceAppend}},
	}
	if err := st.Create(header, seed); err != nil {
		t.Fatalf("create failed: %v", err)
	}
	if err := st.Create(header, nil); err == nil {
		t.Fatal("creating over an existing log must be refused")
	}

	scan, err := st.Load(header.CWD, string(header.ID))
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if scan.Meta.ID != header.ID || len(scan.Events) != 2 {
		t.Fatalf("scan wrong: meta=%+v events=%d", scan.Meta, len(scan.Events))
	}

	if err := st.Append(header.CWD, string(header.ID), scan.CommittedBytes, []session.Event{turnEnd(2)}); err != nil {
		t.Fatalf("append failed: %v", err)
	}
	scan, err = st.Load(header.CWD, string(header.ID))
	if err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	if len(scan.Events) != 3 || scan.Events[2].Type != session.EventTurnEnd {
		t.Fatalf("append not preserved: %d events", len(scan.Events))
	}

	// List finds the session through its header line alone.
	headers, err := st.List()
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if len(headers) != 1 || headers[0].ID != header.ID {
		t.Fatalf("list wrong: %+v", headers)
	}
}

func TestLoadReportsTornTailWithoutMutating(t *testing.T) {
	root := t.TempDir()
	st := &Store{Root: root}
	header := headerFor("torn-tail")
	if err := st.Create(header, []session.Event{turnStart(0), turnEnd(1)}); err != nil {
		t.Fatalf("create failed: %v", err)
	}
	path := st.PathOf(header.CWD, string(header.ID))

	// A crash mid-write leaves a partial final line without a newline.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open failed: %v", err)
	}
	file.WriteString(`{"type":"turn/`)
	file.Close()

	scan, err := st.Load(header.CWD, string(header.ID))
	if err != nil {
		t.Fatalf("load with torn tail failed: %v", err)
	}
	if len(scan.Events) != 2 {
		t.Fatalf("the committed prefix must survive, got %d", len(scan.Events))
	}
	// The scan is non-mutating: the artifact keeps the torn bytes and the
	// scan reports the tail; truncation is the repair's decision.
	if !scan.TornTail {
		t.Fatal("torn tail not reported")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat failed: %v", err)
	}
	if info.Size() <= scan.CommittedBytes {
		t.Fatalf("the torn bytes must remain on load: committed %d, file %d", scan.CommittedBytes, info.Size())
	}

	// The coordinator's repair truncates through CommitRepair; afterwards
	// the next append continues a clean log.
	backend := NewBackend(root, "")
	if err := backend.CommitRepair(header, scan.CommittedBytes, nil); err != nil {
		t.Fatalf("commit repair failed: %v", err)
	}
	if err := st.Append(header.CWD, string(header.ID), scan.CommittedBytes,
		[]session.Event{turnStart(2)}); err != nil {
		t.Fatalf("append after repair failed: %v", err)
	}
	scan, err = st.Load(header.CWD, string(header.ID))
	if err != nil || len(scan.Events) != 3 || scan.TornTail {
		t.Fatalf("repaired log must reload cleanly: %v %d torn=%v", err, len(scan.Events), scan.TornTail)
	}
}

func TestCorruptionBeforeTurnEndIsFatal(t *testing.T) {
	root := t.TempDir()
	st := &Store{Root: root}
	header := headerFor("corrupt")
	// A good header, a good event, an unparsable line, then a complete
	// turn/end — corruption before a turn boundary cannot be a torn write.
	if err := st.Create(header, []session.Event{turnStart(0)}); err != nil {
		t.Fatalf("create failed: %v", err)
	}
	path := st.PathOf(header.CWD, string(header.ID))
	file, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	file.WriteString("{not json\n")
	file.WriteString(`{"type":"turn/end","seq":1,"time":101,"data":{"turn":1,"reason":{"kind":"completed"}}}` + "\n")
	file.Close()

	_, err := st.Load(header.CWD, string(header.ID))
	if err == nil || !strings.Contains(err.Error(), "unparsable committed event at line 2") {
		t.Fatalf("corruption before a turn/end must be fatal, got %v", err)
	}

	// The same corruption WITHOUT a following turn/end is a repairable torn
	// prefix — in a fresh log, so the fatal pair above stays out of it.
	repairable := headerFor("repairable")
	if err := st.Create(repairable, []session.Event{turnStart(0)}); err != nil {
		t.Fatalf("create failed: %v", err)
	}
	rpath := st.PathOf(repairable.CWD, string(repairable.ID))
	file, _ = os.OpenFile(rpath, os.O_WRONLY|os.O_APPEND, 0o644)
	file.WriteString("{also not json\n")
	file.Close()
	scan, scanErr := st.Load(repairable.CWD, string(repairable.ID))
	if scanErr != nil {
		t.Fatalf("load failed: %v", scanErr)
	}
	if len(scan.Events) != 1 {
		t.Fatalf("the prefix before corruption must survive, got %d", len(scan.Events))
	}
}

func TestSeqGapIsFatalBeforeTurnEnd(t *testing.T) {
	root := t.TempDir()
	st := &Store{Root: root}
	header := headerFor("gap")
	if err := st.Create(header, []session.Event{turnStart(0)}); err != nil {
		t.Fatalf("create failed: %v", err)
	}
	path := st.PathOf(header.CWD, string(header.ID))
	file, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	file.WriteString(`{"type":"turn/start","seq":5,"time":105,"data":{"turn":1}}` + "\n")
	file.Close()
	// A gap line without a later turn/end truncates to the preserved prefix.
	scan, err := st.Load(header.CWD, string(header.ID))
	if err != nil {
		t.Fatalf("a gap without a later turn boundary is repairable, got %v", err)
	}
	if len(scan.Events) != 1 {
		t.Fatalf("the in-sequence prefix must survive, got %d", len(scan.Events))
	}

	// A gap followed by a complete turn/end cannot be a torn write: fatal.
	fatalHeader := headerFor("gap-fatal")
	if err := st.Create(fatalHeader, []session.Event{turnStart(0)}); err != nil {
		t.Fatalf("create failed: %v", err)
	}
	fatal := st.PathOf(fatalHeader.CWD, string(fatalHeader.ID))
	file, _ = os.OpenFile(fatal, os.O_WRONLY|os.O_APPEND, 0o644)
	file.WriteString(`{"type":"turn/start","seq":5,"time":105,"data":{"turn":1}}` + "\n")
	file.WriteString(`{"type":"turn/end","seq":6,"time":106,"data":{"turn":1,"reason":{"kind":"completed"}}}` + "\n")
	file.Close()
	_, err = st.Load(fatalHeader.CWD, string(fatalHeader.ID))
	if err == nil || !strings.Contains(err.Error(), "seq gap") {
		t.Fatalf("a seq gap before a turn/end must fail loud, got %v", err)
	}
}

func TestForeignFormatVersionRefusesBeforeStructure(t *testing.T) {
	root := t.TempDir()
	st := &Store{Root: root}
	header := headerFor("future")
	if err := st.Create(header, []session.Event{turnStart(0)}); err != nil {
		t.Fatalf("create failed: %v", err)
	}
	path := st.PathOf(header.CWD, string(header.ID))
	raw, _ := os.ReadFile(path)
	future := strings.Replace(string(raw), `"version":0`, `"version":1`, 1)
	os.WriteFile(path, []byte(future), 0o644)

	_, err := st.Load(header.CWD, string(header.ID))
	var unsupported *SessionFormatUnsupportedError
	if !errors.As(err, &unsupported) || unsupported.Version != 1 {
		t.Fatalf("a future version must refuse as unsupported, got %v", err)
	}
	if !strings.Contains(err.Error(), "upgrade the harness") {
		t.Fatalf("the refusal must tell the user to upgrade, got %q", err.Error())
	}

	// Even a structurally broken future header refuses on version alone.
	broken := strings.Replace(future, `"createdAt":42`, `"createdAt":"not-a-number"`, 1)
	os.WriteFile(path, []byte(broken), 0o644)
	_, err = st.Load(header.CWD, string(header.ID))
	if !errors.As(err, &unsupported) {
		t.Fatalf("version refusal must precede structural checks, got %v", err)
	}
}

func TestParseHeaderMetaRejectsNonHeaders(t *testing.T) {
	header, ok, err := ParseHeaderMeta(`{"type":"session","version":0,"id":"a","createdAt":0,"delegationDepth":0}`)
	if err != nil || !ok || header.ID != "a" {
		t.Fatalf("a valid header line must parse: %v %v %+v", err, ok, header)
	}
	if _, ok, err := ParseHeaderMeta(`{"type":"event"}`); err != nil || ok {
		t.Fatalf("a non-header line is skipped, got ok=%v err=%v", ok, err)
	}
	if _, ok, err := ParseHeaderMeta(`{not json`); err != nil || ok {
		t.Fatalf("an unparsable line is skipped, got ok=%v err=%v", ok, err)
	}
	if _, _, err := ParseHeaderMeta(`{"type":"session","version":3,"id":"a","createdAt":0,"delegationDepth":0}`); err == nil {
		t.Fatal("a foreign version must surface through list too")
	}
}

func TestSessionDirLayout(t *testing.T) {
	root := t.TempDir()
	dir := SessionDir(root, `D:\work`, "s1")
	if dir != filepath.Join(root, "--D-work--", "s1") {
		t.Fatalf("session dir wrong: %q", dir)
	}
	if got := ProjectDir(root, ""); got != filepath.Join(root, "_no-cwd") {
		t.Fatalf("cwd-less sessions group under _no-cwd, got %q", got)
	}
	if got := LogPath(root, `D:\work`, "s1", CompressionNone); !strings.HasSuffix(got, filepath.Join("--D-work--", "s1", "session.jsonl")) {
		t.Fatalf("log path wrong: %q", got)
	}
}

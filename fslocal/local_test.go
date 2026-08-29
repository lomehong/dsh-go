package fslocal

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dshgo/fs"
)

func newTestBackend(t *testing.T) (*Local, string) {
	t.Helper()
	backend, err := New(Config{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	return backend, backend.cwd
}

func mustResolve(t *testing.T, backend *Local, path string) fs.Target {
	t.Helper()
	target, err := backend.Resolve(context.Background(), path, "")
	if err != nil {
		t.Fatalf("resolve %q: %v", path, err)
	}
	return target
}

func TestResolveStableIdentityAndMissingSuffix(t *testing.T) {
	backend, cwd := newTestBackend(t)
	ctx := context.Background()
	file := filepath.Join(cwd, "real.txt")
	if err := os.WriteFile(file, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	direct := mustResolve(t, backend, file)
	// Alias through the same directory identity (clean + different case of
	// separators on Windows) yields the same opaque key.
	dotted := mustResolve(t, backend, filepath.Join(cwd, ".", "real.txt"))
	if direct.Key != dotted.Key {
		t.Fatalf("aliases must share identity: %q vs %q", direct.Key, dotted.Key)
	}
	if direct.DisplayPath != dotted.DisplayPath {
		t.Fatalf("display path must be the joined absolute path: %q vs %q", direct.DisplayPath, dotted.DisplayPath)
	}
	// A missing file still resolves: the key re-appends the missing suffix
	// to the nearest existing ancestor so creation keeps identity stable.
	missing := mustResolve(t, backend, filepath.Join("real.txt", "..", "not-yet", "child.txt"))
	if strings.Contains(string(missing.Key), "..") {
		t.Fatalf("missing-suffix key must be cleaned: %q", missing.Key)
	}
	if !filepath.IsAbs(string(missing.Key)) {
		t.Fatalf("missing-suffix key must be absolute: %q", missing.Key)
	}
	if _, err := backend.Resolve(ctx, "   ", ""); err == nil {
		t.Fatal("blank path must fail loud")
	}
}

func TestStatReadTextAndBinaryRejection(t *testing.T) {
	backend, cwd := newTestBackend(t)
	ctx := context.Background()
	file := filepath.Join(cwd, "text.txt")
	if err := os.WriteFile(file, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := mustResolve(t, backend, file)
	info, err := backend.Stat(ctx, target)
	if err != nil || info == nil {
		t.Fatalf("stat: %v, %v", info, err)
	}
	if info.Type != fs.TypeFile || info.Size == nil || *info.Size != 11 {
		t.Fatalf("stat shape: %+v", info)
	}
	absent, err := backend.Stat(ctx, mustResolve(t, backend, filepath.Join(cwd, "ghost.txt")))
	if err != nil || absent != nil {
		t.Fatalf("absent stat must be nil/nil: %v, %v", absent, err)
	}
	content, err := backend.ReadText(ctx, target)
	if err != nil || content != "alpha\nbeta\n" {
		t.Fatalf("readText: %q, %v", content, err)
	}
	// CRLF is served as stored (normalization is an edit/write concern).
	if err := os.WriteFile(file, []byte("alpha\r\nbeta\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	crlf, err := backend.ReadText(ctx, target)
	if err != nil || crlf != "alpha\r\nbeta\r\n" {
		t.Fatalf("raw read must keep CRLF: %q, %v", crlf, err)
	}
	binary := filepath.Join(cwd, "bin.dat")
	if err := os.WriteFile(binary, []byte{'a', 0, 'b'}, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.ReadText(ctx, mustResolve(t, backend, binary)); err == nil {
		t.Fatal("binary read must fail")
	} else if codeErr, ok := err.(*fs.Error); !ok || codeErr.Code != fs.CodeNotText {
		t.Fatalf("binary read must be FS_NOT_TEXT: %v", err)
	}
}

func TestReadBytesCapFailsTooLarge(t *testing.T) {
	backend, cwd := newTestBackend(t)
	file := filepath.Join(cwd, "blob.bin")
	if err := os.WriteFile(file, make([]byte, 16), 0o644); err != nil {
		t.Fatal(err)
	}
	target := mustResolve(t, backend, file)
	if _, err := backend.ReadBytes(context.Background(), target, 8); err == nil {
		t.Fatal("over-cap read must fail")
	} else if codeErr, ok := err.(*fs.Error); !ok || codeErr.Code != fs.CodeTooLarge {
		t.Fatalf("over-cap read must be FS_TOO_LARGE: %v", err)
	}
	raw, err := backend.ReadBytes(context.Background(), target, 16)
	if err != nil || len(raw) != 16 {
		t.Fatalf("in-cap read: %d bytes, %v", len(raw), err)
	}
}

func TestWriteTextGuardsAndDiffBasis(t *testing.T) {
	backend, cwd := newTestBackend(t)
	ctx := context.Background()
	file := filepath.Join(cwd, "doc.txt")

	// createIfAbsent on an absent file: create outcome, before nil.
	target := mustResolve(t, backend, file)
	created, err := backend.WriteText(ctx, target, "one\r\ntwo\n", &fs.WriteIntent{Kind: fs.IntentCreateIfAbsent}, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Operation != "create" || created.Before != nil {
		t.Fatalf("create outcome: %+v", created)
	}
	if created.After != "one\ntwo\n" {
		t.Fatalf("after must be LF-normalized: %q", created.After)
	}

	// createIfAbsent onto the existing file: FS_NOT_OBSERVED (read first).
	if _, err := backend.WriteText(ctx, target, "x", &fs.WriteIntent{Kind: fs.IntentCreateIfAbsent}, nil); err == nil {
		t.Fatal("createIfAbsent onto existing must fail")
	} else if codeErr, ok := err.(*fs.Error); !ok || codeErr.Code != fs.CodeNotObserved {
		t.Fatalf("must be FS_NOT_OBSERVED: %v", err)
	}

	// replaceIfVersion with a stale version: FS_STALE_VERSION.
	stale := created.Version + "dead"
	if _, err := backend.WriteText(ctx, target, "x", &fs.WriteIntent{Kind: fs.IntentReplaceIfVersion, Version: fs.Version(stale)}, nil); err == nil {
		t.Fatal("stale guard must fail")
	} else if codeErr, ok := err.(*fs.Error); !ok || codeErr.Code != fs.CodeStaleVersion {
		t.Fatalf("must be FS_STALE_VERSION: %v", err)
	}

	// Fresh replaceIfVersion: update outcome with a contextual basis.
	written, err := backend.WriteText(ctx, target, "one\ntwo\nthree\n", &fs.WriteIntent{Kind: fs.IntentReplaceIfVersion, Version: created.Version}, nil)
	if err != nil {
		t.Fatalf("guarded replace: %v", err)
	}
	if written.Operation != "update" || written.Before == nil || *written.Before != "one\ntwo\n" {
		t.Fatalf("update outcome: %+v", written)
	}
	if written.Version == created.Version {
		t.Fatal("version must advance after write")
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("written file: %v", err)
	}
}

func TestEditTextGuardOrderAndUniqueness(t *testing.T) {
	backend, cwd := newTestBackend(t)
	ctx := context.Background()
	file := filepath.Join(cwd, "code.txt")
	target := mustResolve(t, backend, file)
	if err := os.WriteFile(file, []byte("aaa\r\nbbb\r\naaa\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Guard BEFORE matching: a bogus version plus a missing old_string
	// must report FS_STALE_VERSION, not FS_EDIT_NOT_FOUND.
	wrong := fs.Version("0:0:0:0:0")
	if _, err := backend.EditText(ctx, target, fs.EditRequest{OldString: "zzz", NewString: "y"}, &wrong, nil); err == nil {
		t.Fatal("guarded stale edit must fail")
	} else if codeErr, ok := err.(*fs.Error); !ok || codeErr.Code != fs.CodeStaleVersion {
		t.Fatalf("guard must precede matching: %v", err)
	}

	// Ambiguity: two normalized matches, replace_all false.
	if _, err := backend.EditText(ctx, target, fs.EditRequest{OldString: "aaa", NewString: "z"}, nil, nil); err == nil {
		t.Fatal("ambiguous edit must fail")
	} else if codeErr, ok := err.(*fs.Error); !ok || codeErr.Code != fs.CodeAmbiguousEdit {
		t.Fatalf("must be FS_AMBIGUOUS_EDIT: %v", err)
	}

	// replace_all on CRLF content: the match runs on LF-normalized text.
	stat, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	version := versionOf(stat)
	outcome, err := backend.EditText(ctx, target, fs.EditRequest{OldString: "aaa", NewString: "z", ReplaceAll: true}, &version, nil)
	if err != nil {
		t.Fatalf("replace_all edit: %v", err)
	}
	if outcome.Before != "aaa\nbbb\naaa\n" {
		t.Fatalf("before must be LF-normalized: %q", outcome.Before)
	}
	if outcome.After != "z\nbbb\nz\n" {
		t.Fatalf("after: %q", outcome.After)
	}
	if outcome.Version == version {
		t.Fatal("version must advance after edit")
	}
	// The stored file keeps the normalized (LF) content: line-ending
	// restoration is a storage detail this port leaves to consumers.
	stored, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != "z\nbbb\nz\n" {
		t.Fatalf("stored content: %q", string(stored))
	}

	// Missing old_string on current content: FS_EDIT_NOT_FOUND.
	if _, err := backend.EditText(ctx, target, fs.EditRequest{OldString: "ghost", NewString: "y"}, nil, nil); err == nil {
		t.Fatal("missing match must fail")
	} else if codeErr, ok := err.(*fs.Error); !ok || codeErr.Code != fs.CodeEditNotFound {
		t.Fatalf("must be FS_EDIT_NOT_FOUND: %v", err)
	}
	// Empty old_string: FS_EDIT_NOT_FOUND without touching the file.
	if _, err := backend.EditText(ctx, target, fs.EditRequest{OldString: "", NewString: "y"}, nil, nil); err == nil {
		t.Fatal("empty old_string must fail")
	}
}

func TestListDirStableOrderAndEditAbsentIsStale(t *testing.T) {
	backend, cwd := newTestBackend(t)
	ctx := context.Background()
	dir := filepath.Join(cwd, "pkg")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"c.txt", "a.txt", "sub"} {
		full := filepath.Join(dir, name)
		if name == "sub" {
			if err := os.Mkdir(full, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	target := mustResolve(t, backend, dir)
	entries, err := backend.ListDir(ctx, target)
	if err != nil {
		t.Fatalf("listDir: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries: %+v", entries)
	}
	for i, want := range []string{"a.txt", "c.txt", "sub"} {
		if entries[i].Name != want {
			t.Fatalf("row %d = %q, want %q (stable name order)", i, entries[i].Name, want)
		}
	}
	if entries[0].Type != fs.TypeFile || entries[2].Type != fs.TypeDirectory {
		t.Fatalf("types: %+v", entries)
	}
	if entries[0].Target.Key == "" || entries[0].Target.DisplayPath == "" {
		t.Fatalf("child targets must resolve: %+v", entries[0])
	}

	// Editing an absent file reports stale on both guarded and
	// unconditional paths.
	ghost := mustResolve(t, backend, filepath.Join(cwd, "ghost.txt"))
	if _, err := backend.EditText(ctx, ghost, fs.EditRequest{OldString: "a", NewString: "b"}, nil, nil); err == nil {
		t.Fatal("absent edit must fail")
	} else if codeErr, ok := err.(*fs.Error); !ok || codeErr.Code != fs.CodeStaleVersion {
		t.Fatalf("absent edit must be FS_STALE_VERSION: %v", err)
	}
}

func TestContainsAndHostPathMapping(t *testing.T) {
	backend, cwd := newTestBackend(t)
	parent := mustResolve(t, backend, cwd)
	child := mustResolve(t, backend, filepath.Join(cwd, "kid", "f.txt"))
	if !backend.Contains(parent, child) {
		t.Fatal("descendant must be contained")
	}
	if backend.Contains(child, parent) {
		t.Fatal("ancestor must not be contained in the child")
	}
	if backend.ProcessPathFromHostPath("relative/path") != "" {
		t.Fatal("relative host paths map to nothing")
	}
	if backend.ProcessPathFromHostPath(cwd) == "" {
		t.Fatal("absolute host paths map through this host-backed backend")
	}
	if backend.SandboxMode() != "" {
		t.Fatal("the bare local backend never confines by default")
	}
}

func TestStreamTextChunksDecode(t *testing.T) {
	backend, cwd := newTestBackend(t)
	file := filepath.Join(cwd, "stream.txt")
	if err := os.WriteFile(file, []byte(strings.Repeat("data", 5000)), 0o644); err != nil {
		t.Fatal(err)
	}
	target := mustResolve(t, backend, file)
	next, err := backend.StreamText(context.Background(), target)
	if err != nil {
		t.Fatalf("streamText: %v", err)
	}
	var total strings.Builder
	for {
		chunk, ok := next()
		if !ok {
			break
		}
		total.WriteString(chunk)
	}
	if total.Len() != 20000 {
		t.Fatalf("streamed %d bytes", total.Len())
	}
}

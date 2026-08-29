package subprocess

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// helperMain is the re-executed child used by the lifecycle tests: it
// echoes to both streams, drains stdin (when asked), and exits with code 3.
func helperMain() {
	if os.Getenv("DSH_HELPER_SUBPROCESS") == "" {
		return
	}
	mode := os.Getenv("DSH_HELPER_MODE")
	switch mode {
	case "echo":
		fmt.Fprint(os.Stdout, "stdout-line\n")
		fmt.Fprint(os.Stderr, "stderr-line\n")
		os.Exit(3)
	case "spin":
		// A live tree that ignores nothing but outlives any test timeout
		// unless terminated: sleeps in a loop, printing a heartbeat.
		for i := 0; i < 600; i++ {
			time.Sleep(100 * time.Millisecond)
		}
		os.Exit(0)
	case "stdin":
		data, _ := readAllStdin()
		fmt.Fprintf(os.Stdout, "got:%s", data)
		os.Exit(0)
	}
	os.Exit(9)
}

func readAllStdin() (string, error) {
	buf := make([]byte, 0, 256)
	chunk := make([]byte, 64)
	for {
		n, err := os.Stdin.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if err != nil {
			return string(buf), nil
		}
	}
}

func TestMain(m *testing.M) {
	helperMain()
	os.Exit(m.Run())
}

func helperArgv(t *testing.T, mode string) []string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return []string{exe, "-test.run=TestMain", "-test.v=false"}
}

func helperEnv(mode string) map[string]*string {
	tombstone := func(key string) {}
	_ = tombstone
	helperMode := mode
	helper := "1"
	return map[string]*string{
		"DSH_HELPER_MODE":       &helperMode,
		"DSH_HELPER_SUBPROCESS": &helper,
	}
}

func TestSpawnBatchStdinCollectAndExitCode(t *testing.T) {
	handle, err := Spawn(context.Background(), SpawnSpec{
		Argv:    helperArgv(t, "echo"),
		Cwd:     ".",
		Stdio:   Stdio{Stdin: StdinIgnore{}, Stdout: OutputCollect{MaxBytes: 4096}, Stderr: OutputCollect{MaxBytes: 4096}},
		GraceMs: 1000,
		Env:     helperEnv("echo"),
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	outcome, err := handle.Outcome()
	if err != nil {
		t.Fatalf("outcome: %v", err)
	}
	if outcome.ExitCode != 3 || outcome.Signal != "" {
		t.Fatalf("exit facts: %+v", outcome)
	}
	out := handle.CollectedStdout().ReadFrom(0)
	if !strings.Contains(out.Text, "stdout-line") || out.Lossy {
		t.Fatalf("stdout: %+v", out)
	}
	errOut := handle.CollectedStderr().ReadFrom(0)
	if !strings.Contains(errOut.Text, "stderr-line") || errOut.Lossy {
		t.Fatalf("stderr: %+v", errOut)
	}
	if out.NextOffset != len(out.Text) {
		t.Fatalf("next offset: %+v", out)
	}
}

func TestStdinDataBatchAndPipeTermination(t *testing.T) {
	handle, err := Spawn(context.Background(), SpawnSpec{
		Argv:    helperArgv(t, "stdin"),
		Cwd:     ".",
		Stdio:   Stdio{Stdin: StdinData("payload-bytes"), Stdout: OutputCollect{MaxBytes: 4096}, Stderr: OutputInherit{}},
		GraceMs: 1000,
		Env:     helperEnv("stdin"),
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if _, err := handle.Outcome(); err != nil {
		t.Fatalf("outcome: %v", err)
	}
	out := handle.CollectedStdout().ReadFrom(0)
	if !strings.Contains(out.Text, "got:payload-bytes") {
		t.Fatalf("batch stdin roundtrip: %+v", out)
	}
}

func TestStdinPipeFace(t *testing.T) {
	handle, err := Spawn(context.Background(), SpawnSpec{
		Argv:    helperArgv(t, "stdin"),
		Cwd:     ".",
		Stdio:   Stdio{Stdin: StdinPipe{}, Stdout: OutputCollect{MaxBytes: 4096}, Stderr: OutputInherit{}},
		GraceMs: 1000,
		Env:     helperEnv("stdin"),
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if handle.Stdin() == nil {
		t.Fatal("StdinPipe must expose the stdin face")
	}
	if _, err := handle.Stdin().Write([]byte("piped")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := handle.Stdin().Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := handle.Outcome(); err != nil {
		t.Fatalf("outcome: %v", err)
	}
	if !strings.Contains(handle.CollectedStdout().ReadFrom(0).Text, "got:piped") {
		t.Fatalf("piped stdin: %+v", handle.CollectedStdout().ReadFrom(0))
	}
}

func TestTerminateEscalationStopsLiveTree(t *testing.T) {
	started := time.Now()
	handle, err := Spawn(context.Background(), SpawnSpec{
		Argv:    helperArgv(t, "spin"),
		Cwd:     ".",
		Stdio:   Stdio{Stdin: StdinIgnore{}, Stdout: OutputInherit{}, Stderr: OutputInherit{}},
		GraceMs: 200,
		Env:     helperEnv("spin"),
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	handle.Terminate()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if !handle.WaitForExit(ctx) {
		t.Fatal("tree must exit after terminate")
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("termination took %v — escalation failed", elapsed)
	}
	outcome, err := handle.Outcome()
	if err != nil {
		t.Fatalf("outcome: %v", err)
	}
	if runtime.GOOS == "windows" {
		// taskkill /F yields exit code 1.
		if outcome.ExitCode == 0 {
			t.Fatalf("force-terminated tree: %+v", outcome)
		}
	}
	// Idempotence: a second terminate must not panic or hang.
	handle.Terminate()
}

func TestContextAbortTriggersTerminate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	handle, err := Spawn(ctx, SpawnSpec{
		Argv:    helperArgv(t, "spin"),
		Cwd:     ".",
		Stdio:   Stdio{Stdin: StdinIgnore{}, Stdout: OutputInherit{}, Stderr: OutputInherit{}},
		GraceMs: 200,
		Env:     helperEnv("spin"),
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	cancel()
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer waitCancel()
	if !handle.WaitForExit(waitCtx) {
		t.Fatal("abort must terminate the tree")
	}
}

func TestSpecFailLoud(t *testing.T) {
	ctx := context.Background()
	cases := []SpawnSpec{
		{Argv: nil, Cwd: ".", Stdio: fullStdio(), GraceMs: 100},
		{Argv: []string{"  "}, Cwd: ".", Stdio: fullStdio(), GraceMs: 100},
		{Argv: []string{"prog"}, Cwd: "", Stdio: fullStdio(), GraceMs: 100},
		{Argv: []string{"prog"}, Cwd: ".", Stdio: fullStdio(), GraceMs: 0},
		{Argv: []string{"prog"}, Cwd: ".", Stdio: fullStdio(), GraceMs: -5},
		{Argv: []string{"prog"}, Cwd: ".", Stdio: Stdio{}, GraceMs: 100},
	}
	for i, spec := range cases {
		if _, err := Spawn(ctx, spec); err == nil {
			t.Fatalf("case %d must fail loud", i)
		}
	}
	// A pre-cancelled context aborts before start.
	aborted, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := Spawn(aborted, SpawnSpec{Argv: helperArgv(t, "echo"), Cwd: ".", Stdio: fullStdio(), GraceMs: 100, Env: helperEnv("echo")}); err == nil {
		t.Fatal("pre-aborted context must fail the spawn")
	}
}

func fullStdio() Stdio {
	return Stdio{Stdin: StdinIgnore{}, Stdout: OutputInherit{}, Stderr: OutputInherit{}}
}

func TestRawPipeStreamBelongsToCaller(t *testing.T) {
	handle, err := Spawn(context.Background(), SpawnSpec{
		Argv:    helperArgv(t, "echo"),
		Cwd:     ".",
		Stdio:   Stdio{Stdin: StdinIgnore{}, Stdout: OutputPipe{}, Stderr: OutputPipe{}},
		GraceMs: 1000,
		Env:     helperEnv("echo"),
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if handle.Stdout() == nil || handle.Stderr() == nil {
		t.Fatal("pipe mode must expose raw streams")
	}
	data := make([]byte, 256)
	n, _ := handle.Stdout().Read(data)
	if !strings.Contains(string(data[:n]), "stdout-line") {
		t.Fatalf("raw stdout: %q", string(data[:n]))
	}
	if _, err := handle.Outcome(); err != nil {
		t.Fatalf("outcome: %v", err)
	}
}

func TestChildEnvScrubAndTombstones(t *testing.T) {
	t.Setenv("DSH_SECRET_FACT", "leak")
	t.Setenv("AMBIENT_VAR", "keep")
	t.Setenv("DEEPSEEK_API_KEY", "credential-shaped")
	t.Setenv("MY_TOKEN", "also-shaped")
	optIn := "restored"
	env := ChildEnv(map[string]*string{
		"DSH_SECRET_FACT": &optIn, // explicit opt-in survives the scrub
		"AMBIENT_VAR":     nil,    // tombstone removes the ambient entry
		"EXTRA":           ptrStr("added"),
	})
	if env["DSH_SECRET_FACT"] != "restored" {
		t.Fatalf("opt-in: %v", env["DSH_SECRET_FACT"])
	}
	if _, has := env["AMBIENT_VAR"]; has {
		t.Fatal("tombstone must remove the ambient entry")
	}
	if env["EXTRA"] != "added" {
		t.Fatalf("extra: %v", env["EXTRA"])
	}
	plain := ScrubbedParentEnv()
	if _, has := plain["DSH_SECRET_FACT"]; has {
		t.Fatal("scrub must drop managed facts")
	}
	if _, has := plain["DEEPSEEK_API_KEY"]; has {
		t.Fatal("scrub must drop credential-shaped names")
	}
	if _, has := plain["MY_TOKEN"]; has {
		t.Fatal("scrub must drop TOKEN-shaped names")
	}
	if plain["AMBIENT_VAR"] != "keep" {
		t.Fatalf("ambient: %v", plain["AMBIENT_VAR"])
	}
}

func ptrStr(v string) *string { return &v }

func TestCollectorTailTruncationAndLossyReads(t *testing.T) {
	collector := newOutputCollector(10, 0, "stdout", t.TempDir())
	collector.push([]byte("0123456789"))
	collector.push([]byte("abcde"))
	final := collector.finalize()
	if final.Text != "56789abcde" || !final.Truncated {
		t.Fatalf("tail: %+v", final)
	}
	// ReadFrom(0) after loss: whole tail, lossy flag, and the total offset.
	read := collector.readFrom(0)
	if !read.Lossy || read.Text != "56789abcde" || read.NextOffset != 15 {
		t.Fatalf("lossy: %+v", read)
	}
	// An in-window read returns the delta only.
	collector2 := newOutputCollector(100, 0, "stdout", t.TempDir())
	collector2.push([]byte("hello"))
	collector2.push([]byte(" world"))
	read2 := collector2.readFrom(5)
	if read2.Lossy || read2.Text != " world" || read2.NextOffset != 11 {
		t.Fatalf("delta: %+v", read2)
	}
	// An offset past the end is an empty delta at the same offset.
	read3 := collector2.readFrom(50)
	if read3.Lossy || read3.Text != "" || read3.NextOffset != 11 {
		t.Fatalf("past-end: %+v", read3)
	}
}

func TestCollectorSpillLifecycle(t *testing.T) {
	dir := t.TempDir()
	spillDirOverride = dir
	defer func() { spillDirOverride = "" }()
	collector := newOutputCollector(4, 64, "stdout", dir)
	collector.push([]byte("0123456789")) // spills from first overflow
	read := collector.readFrom(0)
	if read.SpillPath == "" {
		t.Fatal("spill must be advertised while within cap")
	}
	if _, err := os.Stat(read.SpillPath); err != nil {
		t.Fatalf("spill file: %v", err)
	}
	// Exceeding the whole-stream cap discards the now-incomplete spill.
	collector.push([]byte(strings.Repeat("x", 100)))
	final := collector.finalize()
	if final.SpillPath != "" {
		t.Fatalf("discarded spill must stop being advertised: %+v", final)
	}
	if _, err := os.Stat(read.SpillPath); !os.IsNotExist(err) {
		t.Fatal("discarded spill file must be removed")
	}
	// Sealing closes the spill file; a SUCCESSFUL close keeps advertising
	// the (complete, intact) path — only a failed close stops advertising.
	collector2 := newOutputCollector(4, 64, "stderr", dir)
	collector2.push([]byte("abcdef"))
	collector2.seal()
	sealed := collector2.finalize()
	if sealed.SpillPath == "" || !sealed.Truncated || sealed.Text != "cdef" {
		t.Fatalf("sealed: %+v", sealed)
	}
	if _, err := os.Stat(sealed.SpillPath); err != nil {
		t.Fatalf("intact spill must survive seal: %v", err)
	}
	if spillDirOverride != "" { // quiet the unused guard on some platforms
		_ = filepath.Join(dir, "x")
	}
}

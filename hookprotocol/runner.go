// Runner ports hook-protocol/src/runner.ts: execute command hooks with the
// trusted stdin payload and dialect environment, then decode the captured
// outcome.
//
// Go adaptation: the official routes through the dsh-shell capability (its
// credential scrub and process-group cancellation). The Go port has no
// shell service, so this runner owns execution directly — a timeout child
// context cancels the run, env entries merge over os.Environ, and the
// command resolves through the platform shell (cmd /c on Windows, sh -c
// elsewhere) via the resolveInvocation seam tests can replace.
package hookprotocol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// DefaultHookTimeoutMs is the reference default per-hook timeout, in
// milliseconds (10 minutes) — the value both Claude Code and Codex apply to
// a hook whose config sets no timeout. It lives here, once, as the
// protocol's default; the bridges' DefaultTimeoutMs config defaults to it,
// and a per-hook CommandHook.TimeoutSec is the override API.
const DefaultHookTimeoutMs = 600_000

// RunHookOptions is everything a single hook invocation needs beyond its
// command line.
type RunHookOptions struct {
	// Payload is the JSON payload object written to the hook's stdin (the
	// bridge builds it).
	Payload any
	// Env holds extra env vars for the hook process (CLAUDE_PROJECT_DIR,
	// …); the bridge builds these.
	Env map[string]string
	// CWD is the working directory for the hook (the executor's own default
	// when empty).
	CWD string
	// Signal is the explicit owning-operation context; firing it cancels
	// the hook run.
	Signal context.Context
	// TrailingNewline appends a trailing newline to the stdin payload
	// (CC yes, Codex no).
	TrailingNewline bool
	// DefaultTimeoutMs applies when the hook's config sets no timeout of
	// its own. The bridge owns the default (its DefaultTimeoutMs config,
	// reference default DefaultHookTimeoutMs) and passes it in explicitly.
	DefaultTimeoutMs int64
	// ExpectedEventName is the event this hook is firing for (e.g.
	// "PreToolUse"). When set, a structured hookSpecificOutput block whose
	// hookEventName names a DIFFERENT event is treated as malformed and its
	// event-scoped fields are discarded (see ParseHookOutput). Empty
	// applies any block as-is.
	ExpectedEventName string
}

// RunHookResult is the HookOutput plus the wall-clock duration of the run
// (for hook/result).
type RunHookResult struct {
	// Output is the decoded outcome.
	Output HookOutput
	// DurationMs is the wall-clock duration of the run, from now — durable
	// on the hook/result event.
	DurationMs int64
}

// resolveInvocation adapts a command line to a process invocation. The
// default resolves through the platform shell; tests replace it.
var resolveInvocation = defaultResolveInvocation

func defaultResolveInvocation(command string) (name string, args []string) {
	if os.PathSeparator == '\\' {
		return "cmd", []string{"/c", command}
	}
	return "sh", []string{"-c", command}
}

// RunHook runs hook with serialized stdin and decodes its outcome. A
// hook-specific timeout in seconds overrides the default; trusted
// environment entries merge after the process environment. Infrastructure
// rejection becomes an outcome with no exit code, so this function never
// panics or crashes the calling turn.
//
// now is the millisecond clock used for the reported duration.
func RunHook(hook CommandHook, options RunHookOptions, now func() int64) RunHookResult {
	started := now()
	timeoutMs := options.DefaultTimeoutMs
	if hook.TimeoutSec != nil {
		timeoutMs = int64(*hook.TimeoutSec * 1000)
	}
	stdinBytes, err := json.Marshal(options.Payload)
	if err != nil {
		// The payload comes from the bridge's map literals; a marshal
		// failure is an infrastructure fault with no exit code.
		return RunHookResult{
			Output:     ParseHookOutput(nil, "", err.Error(), ""),
			DurationMs: now() - started,
		}
	}
	if options.TrailingNewline {
		stdinBytes = append(stdinBytes, '\n')
	}

	parent := options.Signal
	if parent == nil {
		parent = context.Background()
	}
	runCtx, cancel := context.WithTimeout(parent, time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()

	name, args := resolveInvocation(hook.Command)
	// Plain Command: expiry/signal handling goes through the watcher below,
	// whose tree kill needs the shell process still alive (CommandContext's
	// direct-child kill would orphan the shell's grandchildren first).
	cmd := exec.Command(name, args...)
	if options.CWD != "" {
		cmd.Dir = options.CWD
	}
	if len(options.Env) > 0 {
		cmd.Env = os.Environ()
		for key, value := range options.Env {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdin = bytes.NewReader(stdinBytes)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// The platform shell spawns grandchildren that inherit the captured
	// pipes, so expiry/signal needs a TREE kill (taskkill /T on Windows,
	// process kill elsewhere) for the pipes to close promptly — the
	// official runner gets the same effect from the shell capability's
	// process-group cancellation.
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-runCtx.Done():
			if cmd.Process != nil {
				if os.PathSeparator == '\\' {
					kill := exec.Command("taskkill", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F")
					_ = kill.Run()
				} else {
					_ = cmd.Process.Kill()
				}
			}
		case <-watchDone:
		}
	}()
	runErr := cmd.Run()

	// The protocol's exit-code contract is numeric: a signal death or a
	// timeout maps to nil (a non-blocking outcome — no clean exit code to
	// act on), matching ShellRunResult's null exitCode handling.
	var exitCode *int
	wasExitError := false
	if runErr == nil {
		zero := 0
		exitCode = &zero
	} else if runCtx.Err() == nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			wasExitError = true
			code := exitErr.ExitCode()
			if code >= 0 {
				exitCode = &code
			}
		}
	}
	if exitCode != nil {
		return RunHookResult{
			Output:     ParseHookOutput(exitCode, stdout.String(), stderr.String(), options.ExpectedEventName),
			DurationMs: now() - started,
		}
	}
	// A hook that cannot run (or died by signal/timeout) is a non-blocking
	// error: no exit code; a spawn failure carries its message on stderr
	// for the record. The turn proceeds.
	message := ""
	if runCtx.Err() == nil && !wasExitError {
		message = runErr.Error()
	}
	return RunHookResult{
		Output:     ParseHookOutput(nil, "", message, ""),
		DurationMs: now() - started,
	}
}

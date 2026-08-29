package subagent

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"

	"dshgo/agent"
	"dshgo/llm"
	"dshgo/session"
)

// Provider-side vocabulary for OUT-OF-PROCESS subagent backends (official
// out-of-process.ts) — the pieces that enforce this seam's own contracts
// around a child in another process: the no-capabilities advertisement,
// timing-bound validation, child working-directory resolution, the
// never-reject result settlement, and the standard run-handle publication.
// The process machinery itself (spawn, env scrub, tree-scoped teardown)
// belongs to the subprocess seam.

// maxSubagentDiagnosticBytes is the maximum UTF-8 size of a
// SubagentResult diagnostic.
const maxSubagentDiagnosticBytes = 4096

// diagnosticTruncationSuffix marks a visibly truncated diagnostic.
const diagnosticTruncationSuffix = "\n[diagnostic truncated]"

// limitSubagentDiagnostic limits provider-authored failure detail without
// splitting a UTF-8 sequence.
func limitSubagentDiagnostic(diagnostic string) string {
	bytes := []byte(diagnostic)
	if len(bytes) <= maxSubagentDiagnosticBytes {
		return diagnostic
	}
	suffixBytes := len(diagnosticTruncationSuffix)
	prefixBytes := maxSubagentDiagnosticBytes - suffixBytes
	// Step back over continuation bytes so the cut lands on a lead byte.
	for prefixBytes > 0 && bytes[prefixBytes]&0b1100_0000 == 0b1000_0000 {
		prefixBytes--
	}
	return string(bytes[:prefixBytes]) + diagnosticTruncationSuffix
}

// normalizeSubagentDiagnostic enforces the byte limit on a provider-returned
// diagnostic. The empty-string diagnostic and an absent one are the same
// no-value on the Go side (string ↔ optional).
func normalizeSubagentDiagnostic(result SubagentResult) SubagentResult {
	if result.Diagnostic == "" {
		return result
	}
	result.Diagnostic = limitSubagentDiagnostic(result.Diagnostic)
	return result
}

// NoStartCapabilities is the capability advertisement of an out-of-process
// backend: NONE. A child in another process cannot honor parent-enforced
// start features (agentOptions/outputSchema/maxDepth/toolFilter/persona), so
// the service rejects a request needing any of them before start runs —
// never accepted-then-ignored.
func NoStartCapabilities() SubagentCapabilities {
	return SubagentCapabilities{
		AgentOptions: false,
		OutputSchema: false,
		DepthLimit:   false,
		ToolFilter:   false,
		Persona:      false,
	}
}

// AssertPositiveFinite asserts a configured timing bound is a positive finite
// number (it bounds a teardown or shutdown wait; zero, negative, or NaN would
// skip or wedge it).
func AssertPositiveFinite(prefix string, name string, value float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
		return fmt.Errorf("%s: %s must be a positive finite number", prefix, name)
	}
	return nil
}

// isEnterableDirectory reports whether path names an existing directory the
// harness can ENTER. The search-permission probe matters: stat
// IsDirectory() is true for a no-execute directory, but a subprocess cwd
// needs it or spawn fails EACCES.
func isEnterableDirectory(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	// Opening the directory is the portable enter probe; every error class
	// (ENOENT/EACCES/ENOTDIR/…) means the path cannot serve as the child's
	// cwd.
	handle, err := os.Open(path)
	if err != nil {
		return false
	}
	return handle.Close() == nil
}

// AssertUsableCwd asserts cwd can actually host the child: absolute (it
// doubles as the child's workspace identity, and a relative path would be
// re-anchored to the server process's launch directory) and an existing
// enterable directory (fail here, before the process boundary, instead of as
// an ambiguous spawn ENOENT).
func AssertUsableCwd(prefix string, label string, cwd string) (string, error) {
	if !filepath.IsAbs(cwd) {
		return "", fmt.Errorf("%s: %s must be an absolute path: %s", prefix, label, cwd)
	}
	if !isEnterableDirectory(cwd) {
		return "", fmt.Errorf("%s: %s is not an accessible directory: %s", prefix, label, cwd)
	}
	return cwd, nil
}

// ValidateConfiguredCwd validates a configured cwd override ONCE, at plugin
// load: reject the empty string (it would silently reintroduce the
// launch-directory fallback this resolution removes), interpret a relative
// path against the harness launch directory, and require an enterable
// directory. present=false is the omitted config key.
func ValidateConfiguredCwd(prefix string, cwd string, present bool) (string, error) {
	if !present {
		return "", nil
	}
	if cwd == "" {
		return "", fmt.Errorf("%s: config cwd must not be empty — omit the key to inherit the parent session cwd", prefix)
	}
	resolved, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("%s: config cwd cannot be resolved: %s", prefix, cwd)
	}
	return AssertUsableCwd(prefix, "config cwd", resolved)
}

// ResolveChildCwd resolves the child's working directory at start: the
// deployment override when configured (already validated at load), else the
// parent session's workspace cwd (validated here, its earliest resolvable
// point). Fails loud when neither exists — falling back to the harness
// process cwd would silently bind the child to the server's launch directory
// instead of the delegating session's workspace (one server process serves
// many sessions, each with its own cwd).
func ResolveChildCwd(prefix string, configured string, configuredPresent bool, parentCwd string, parentPresent bool) (string, error) {
	if configuredPresent {
		return configured, nil
	}
	if !parentPresent {
		return "", fmt.Errorf("%s: no working directory for the child — configure `cwd` or delegate from a parent session that has one", prefix)
	}
	return AssertUsableCwd(prefix, "parent session cwd", parentCwd)
}

// RunResultSettlement carries the inputs to SettleRunResult.
type RunResultSettlement struct {
	// Attempt is the turn attempt (typically racing local cancellation).
	Attempt func() (SubagentResult, error)
	// CollectOutput is the snapshot the provider exposes when cancellation
	// or failure wins settlement.
	CollectOutput func() []llm.ContentBlock
	// CollectDiagnostic snapshots safe provider-authored detail when a
	// failure wins settlement.
	CollectDiagnostic func() (string, bool)
	// Cancelled reports whether local cancellation settled before the
	// attempt's outcome is observed.
	Cancelled func() bool
	// OnError is the diagnostic sink for a failure flattened to a stop
	// reason; a throw from it is contained.
	OnError func(err error, stopReason StopReason)
	// Done closes when the run settles, releasing the abort watcher.
	Done chan struct{}
	// Cancelled-ch wiring: Abort registration is the caller's start concern;
	// once releases it at settlement.
	Once *sync.Once
	// StopAbort releases the caller's abort watcher at settlement.
	StopAbort func()
}

// SettleRunResult settles an out-of-process run result under the seam
// contract: the result never rejects after publication. A normally completed
// or failed attempt resolves as aborted when cancellation already settled
// locally; another failure is flattened to StopError through the contained
// diagnostic sink. Provider-returned diagnostics use the same byte limit.
// The abort watcher is released on every path.
func SettleRunResult(parts RunResultSettlement) (result SubagentResult) {
	defer func() {
		if parts.StopAbort != nil {
			parts.StopAbort()
		}
		if parts.Once != nil && parts.Done != nil {
			parts.Once.Do(func() { close(parts.Done) })
		}
	}()
	attempt, err := parts.Attempt()
	if err == nil {
		if parts.Cancelled() {
			return SubagentResult{Output: parts.CollectOutput(), StopReason: StopAborted}
		}
		return normalizeSubagentDiagnostic(attempt)
	}
	// Cover a failure already queued when cancellation arrives.
	if parts.Cancelled() {
		return SubagentResult{Output: parts.CollectOutput(), StopReason: StopAborted}
	}
	// Flatten post-publication transport failures while preserving
	// diagnostics. The diagnostic sink cannot reject the run result.
	if parts.OnError != nil {
		func() {
			defer func() { _ = recover() }()
			parts.OnError(err, StopError)
		}()
	}
	output := SubagentResult{Output: parts.CollectOutput(), StopReason: StopError}
	if parts.CollectDiagnostic != nil {
		if collected, ok := parts.CollectDiagnostic(); ok {
			output.Diagnostic = limitSubagentDiagnostic(collected)
		}
	}
	return output
}

// SubprocessRunHandleParts carries the inputs to SubprocessRunHandle.
type SubprocessRunHandleParts struct {
	// ID is the parent-scoped run id.
	ID session.SessionID
	// Result is the flattened, never-rejecting result (the seam contract).
	Result func() (SubagentResult, error)
	// RequestCancel settles local cancellation so Result resolves without
	// the child.
	RequestCancel func()
	// Teardown tears the child process down to quiescence (backend-owned
	// ladder).
	Teardown func() error
	// Done closes when the run settles, releasing the abort watcher.
	Done chan struct{}
	// Once guards the single Done close.
	Once *sync.Once
	// StopAbort releases the caller's abort watcher at disposal.
	StopAbort func()
}

// subprocessRunHandleState memoizes the one disposal.
type subprocessRunHandleState struct {
	once     sync.Once
	disposal error
}

// SubprocessRunHandle publishes the seam run handle for an out-of-process
// child. Dispose is idempotent (one memoized teardown): it releases the
// abort watcher, settles local cancellation — there is no assumption the
// child cooperates — and then awaits the backend's teardown to actual exit.
// LocalAgent is nil for remote runs.
func SubprocessRunHandle(parts SubprocessRunHandleParts) SubagentRun {
	state := &subprocessRunHandleState{}
	return &subprocessRun{
		id:            parts.ID,
		result:        parts.Result,
		done:          parts.Done,
		once:          parts.Once,
		stopAbort:     parts.StopAbort,
		requestCancel: parts.RequestCancel,
		teardown:      parts.Teardown,
		state:         state,
	}
}

// subprocessRun is the SubagentRun implementation over the flattened result
// and the memoized teardown.
type subprocessRun struct {
	id            session.SessionID
	result        func() (SubagentResult, error)
	done          chan struct{}
	once          *sync.Once
	stopAbort     func()
	requestCancel func()
	teardown      func() error
	state         *subprocessRunHandleState
}

// ID implements SubagentRun.
func (r *subprocessRun) ID() session.SessionID { return r.id }

// LocalAgent implements SubagentRun: nil for remote runs.
func (r *subprocessRun) LocalAgent() *agent.Agent { return nil }

// Result implements SubagentRun.
func (r *subprocessRun) Result() (SubagentResult, error) { return r.result() }

// Dispose implements SubagentRun.
func (r *subprocessRun) Dispose() error {
	if r.stopAbort != nil {
		r.stopAbort()
	}
	if r.once != nil && r.done != nil {
		r.once.Do(func() { close(r.done) })
	}
	r.requestCancel()
	r.state.once.Do(func() {
		r.state.disposal = r.teardown()
	})
	return r.state.disposal
}

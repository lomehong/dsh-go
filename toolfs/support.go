package toolfs

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"dshgo/agent"
	"dshgo/fs"
	"dshgo/sandbox"
)

// parentPathSegment matches a `..` path component; traversal makes a
// symlinked cwd's filesystem identity observable, so such cwds canonicalize.
var parentPathSegment = regexp.MustCompile(`(?:^|[\\/])\.\.(?:[\\/]|$)`)

// AgentSource resolves the calling agent from one execution's scope key (the
// established resolveByScope pattern; nil for agent-less calls).
type AgentSource interface {
	ResolveAgent(scope any) *agent.Agent
}

// RegistryAgentSource adapts the agent registry.
type RegistryAgentSource struct {
	Registry *agent.AgentRegistry
}

// ResolveAgent matches one scope key against the live registry.
func (r RegistryAgentSource) ResolveAgent(scope any) *agent.Agent {
	if r.Registry == nil || scope == nil {
		return nil
	}
	key, ok := scope.(agent.ScopeKey)
	if !ok {
		return nil
	}
	return r.Registry.ByScope(key)
}

// sessionCwd derives the working directory a filesystem tool resolves
// relative paths against: the calling agent's per-session workspace, so each
// session's read/write/edit act on its workspace, not the server's launch
// directory. Agent-less calls return "" leaving the fallback in the provider.
func sessionCwd(source AgentSource, scope any, requestedPath string) string {
	if source == nil {
		return ""
	}
	caller := source.ResolveAgent(scope)
	if caller == nil || caller.Session == nil {
		return ""
	}
	cwd := caller.Session.Header().CWD
	if cwd == "" || (!parentPathSegment.MatchString(cwd) && !parentPathSegment.MatchString(requestedPath)) {
		return cwd
	}
	return sandbox.CanonicalPath(cwd)
}

// parsePositiveInteger validates the value constraints the schema DSL cannot
// express.
func parsePositiveInteger(value int, name string) (int, error) {
	if value < 1 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return value, nil
}

// RemediateFsError appends the correct recovery instruction to a
// guarded-mutation failure's message: FS_STALE_VERSION recovers only by
// re-reading; FS_NOT_OBSERVED by reading. The code is preserved so retry and
// UI layers keep routing on it; anything else passes through untouched.
func RemediateFsError(err error) error {
	codeErr, ok := err.(*fs.Error)
	if !ok {
		return err
	}
	var remedy string
	switch codeErr.Code {
	case fs.CodeStaleVersion:
		remedy = "re-read the file, then retry"
	case fs.CodeNotObserved:
		remedy = "read the file, then retry"
	default:
		return err
	}
	return fs.NewError(codeErr.Code, fmt.Sprintf("%s — %s", codeErr.Detail, remedy), err)
}

// blankArgs reads one string argument treating an absent key, a null, or a
// non-string as blank.
func blankArg(args map[string]any, key string) bool {
	raw, ok := args[key]
	if !ok {
		return true
	}
	value, ok := raw.(string)
	return !ok || strings.TrimSpace(value) == ""
}

// blankPath validates the value constraint the schema DSL cannot express.
func blankPath(args map[string]any) error {
	if blankArg(args, "file_path") {
		return fmt.Errorf("file_path must be a non-empty string")
	}
	return nil
}

func argString(args map[string]any, key string) string {
	value, _ := args[key].(string)
	return value
}

func argInt(args map[string]any, key string) (int, bool) {
	raw, ok := args[key].(float64)
	if !ok {
		return 0, false
	}
	return int(raw), true
}

func argBool(args map[string]any, key string) bool {
	value, _ := args[key].(bool)
	return value
}

// signalOf extracts the execution signal, defaulting to background.
func signalOf(execSignal context.Context) context.Context {
	if execSignal != nil {
		return execSignal
	}
	return context.Background()
}

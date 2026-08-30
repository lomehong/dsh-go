package shell

import (
	"context"
	"strings"
	"testing"

	"dshgo/agent"
	"dshgo/session"
	"dshgo/tools"
)

// newShellTestAgent mints a detached agent the way agent tests do.
func newShellTestAgent(t *testing.T, id string) *agent.Agent {
	t.Helper()
	sess, err := session.NewDetached(session.SessionID(id), nil, nil)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	return agent.NewAgent(agent.AgentConfig{ID: session.SessionID(id), Session: sess}, nil)
}

func TestShellEnvRegistryRegisterFailLoud(t *testing.T) {
	registry := NewShellEnvRegistry("D:\\dsh-home", nil)
	valid := BashEnvContributor{
		Name: "hooks",
		Variables: map[string]BashEnvVariable{
			"DSH_HOOK_KIND": {Description: "the hook kind"},
		},
		Resolve: func(*tools.ToolExecution) map[string]string { return nil },
	}
	undo, err := registry.Register(valid)
	if err != nil {
		t.Fatalf("valid: %v", err)
	}
	// Duplicate name.
	dup := valid
	if _, err := registry.Register(dup); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("duplicate name: %v", err)
	}
	// Invalid key shape.
	badKey := BashEnvContributor{Name: "other", Variables: map[string]BashEnvVariable{"DSH_lowercase": {Description: "x"}}, Resolve: func(*tools.ToolExecution) map[string]string { return nil }}
	if _, err := registry.Register(badKey); err == nil || !strings.Contains(err.Error(), "invalid key") {
		t.Fatalf("invalid key: %v", err)
	}
	noPrefix := BashEnvContributor{Name: "other", Variables: map[string]BashEnvVariable{"HOME": {Description: "x"}}, Resolve: func(*tools.ToolExecution) map[string]string { return nil }}
	if _, err := registry.Register(noPrefix); err == nil || !strings.Contains(err.Error(), "invalid key") {
		t.Fatalf("missing prefix: %v", err)
	}
	// Reserved keys.
	reserved := BashEnvContributor{Name: "other", Variables: map[string]BashEnvVariable{"DSH_HOME": {Description: "x"}}, Resolve: func(*tools.ToolExecution) map[string]string { return nil }}
	if _, err := registry.Register(reserved); err == nil || !strings.Contains(err.Error(), "reserved key") {
		t.Fatalf("reserved: %v", err)
	}
	// Blank description.
	blank := BashEnvContributor{Name: "other", Variables: map[string]BashEnvVariable{"DSH_THING": {Description: "  "}}, Resolve: func(*tools.ToolExecution) map[string]string { return nil }}
	if _, err := registry.Register(blank); err == nil || !strings.Contains(err.Error(), `must describe "DSH_THING"`) {
		t.Fatalf("blank description: %v", err)
	}
	// Key ownership conflict.
	conflict := BashEnvContributor{Name: "other", Variables: map[string]BashEnvVariable{"DSH_HOOK_KIND": {Description: "x"}}, Resolve: func(*tools.ToolExecution) map[string]string { return nil }}
	if _, err := registry.Register(conflict); err == nil || !strings.Contains(err.Error(), "already owned by contributor") {
		t.Fatalf("conflict: %v", err)
	}
	// Blank name.
	if _, err := registry.Register(BashEnvContributor{Name: " "}); err == nil || !strings.Contains(err.Error(), "non-empty") {
		t.Fatalf("blank name: %v", err)
	}
	// Disposal frees the keys and the name.
	undo()
	if _, err := registry.Register(valid); err != nil {
		t.Fatalf("re-register after disposal: %v", err)
	}
}

func TestShellEnvRegistryCollectBuiltinsAndOrder(t *testing.T) {
	caller := newShellTestAgent(t, "shell-env-owner")
	resolver := func(scope tools.ScopeKey) *agent.Agent {
		if scope == nil {
			return nil
		}
		if scope == caller.Scope {
			return caller
		}
		return nil
	}
	envRegistry := NewShellEnvRegistry("D:\\dsh-home", resolver)

	second := BashEnvContributor{
		Name:      "zeta",
		Variables: map[string]BashEnvVariable{"DSH_ZETA": {Description: "z fact"}},
		Resolve:   func(*tools.ToolExecution) map[string]string { return map[string]string{"DSH_ZETA": "z"} },
	}
	first := BashEnvContributor{
		Name:      "alpha",
		Variables: map[string]BashEnvVariable{"DSH_ALPHA": {Description: "a fact"}},
		Resolve: func(*tools.ToolExecution) map[string]string {
			return map[string]string{"DSH_ALPHA": "a"}
		},
	}
	// Register zeta first; collection still resolves in NAME order
	// (alpha < zeta), so each key keeps its own contributor's value.
	undoZ, err := envRegistry.Register(second)
	if err != nil {
		t.Fatal(err)
	}
	undoA, err := envRegistry.Register(first)
	if err != nil {
		t.Fatal(err)
	}
	defer undoZ()
	defer undoA()

	collected := envRegistry.Collect(&tools.ToolExecution{Agent: caller.Scope})
	if collected["DSH_HOME"] != "D:\\dsh-home" || collected["DSH_SHELL"] != "1" {
		t.Fatalf("builtins: %v", collected)
	}
	if collected["DSH_SESSION_ID"] == "" {
		t.Fatalf("session id: %v", collected)
	}
	if collected["DSH_ALPHA"] != "a" || collected["DSH_ZETA"] != "z" {
		t.Fatalf("contributor order: %v", collected)
	}

	// Agent-less executions carry no DSH_SESSION_ID.
	agentless := envRegistry.Collect(&tools.ToolExecution{})
	if _, has := agentless["DSH_SESSION_ID"]; has {
		t.Fatalf("agentless session id: %v", agentless)
	}

	// List enumerates declarations sorted by key without resolving.
	listed := envRegistry.List()
	if len(listed) != 2 || listed[0].Key != "DSH_ALPHA" || listed[1].Key != "DSH_ZETA" {
		t.Fatalf("list: %+v", listed)
	}
	if listed[0].Contributor != "alpha" || listed[0].Description != "a fact" {
		t.Fatalf("list metadata: %+v", listed[0])
	}
}

func TestShellEnvRegistryCollectPanicsOnUndeclared(t *testing.T) {
	envRegistry := NewShellEnvRegistry("D:\\dsh-home", nil)
	rogue := BashEnvContributor{
		Name:      "rogue",
		Variables: map[string]BashEnvVariable{"DSH_DECLARED": {Description: "declared"}},
		Resolve:   func(*tools.ToolExecution) map[string]string { return map[string]string{"DSH_UNDECLARED": "x"} },
	}
	if _, err := envRegistry.Register(rogue); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if recovered := recover(); recovered == nil {
			t.Fatal("undeclared return must fail loud")
		}
	}()
	envRegistry.Collect(&tools.ToolExecution{})
}

func TestParseExitStatusMarkers(t *testing.T) {
	// Exit marker split: body cut at the match start (the blank line's
	// first newline stays; the marker's own leading newline goes).
	parsed := ParseExitStatus("output line 1\noutput line 2\n\n[exit code: 3]")
	if parsed.Body != "output line 1\noutput line 2\n" || parsed.ExitCode != 3 || parsed.Signal != "" {
		t.Fatalf("exit: %+v", parsed)
	}
	// Killed marker wins over everything.
	killed := ParseExitStatus("partial output\n\n[killed by signal: SIGTERM]")
	if killed.Body != "partial output\n" || killed.Signal != "SIGTERM" {
		t.Fatalf("killed: %+v", killed)
	}
	// Absent markers mean a clean exit 0 and the body passes through.
	clean := ParseExitStatus("all good")
	if clean.Body != "all good" || clean.ExitCode != 0 || clean.Signal != "" {
		t.Fatalf("clean: %+v", clean)
	}
	// Marker-like text that is NOT the final line stays in the body.
	tricky := ParseExitStatus("[exit code: 9]\nreal output")
	if tricky.ExitCode != 0 || tricky.Body != "[exit code: 9]\nreal output" {
		t.Fatalf("tricky: %+v", tricky)
	}
	// A marker without the leading newline (start of string) does not match.
	if got := ParseExitStatus("[exit code: 5]"); got.ExitCode != 0 || got.Body != "[exit code: 5]" {
		t.Fatalf("no leading newline: %+v", got)
	}
}

func TestShellEnvRegistryDefaultHome(t *testing.T) {
	t.Setenv("DSH_HOME", "D:\\explicit-home")
	registry := NewShellEnvRegistry("", nil)
	collected := registry.Collect(&tools.ToolExecution{})
	if collected["DSH_HOME"] != "D:\\explicit-home" {
		t.Fatalf("env default: %v", collected["DSH_HOME"])
	}
	_ = context.Background()
}

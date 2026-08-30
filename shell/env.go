package shell

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"dshgo/agent"
	"dshgo/tools"
)

// DSH_HOME exposes the harness home directory to model shell calls.
const DshHomeEnv = "DSH_HOME"

// dshShellKey marks a shell executed through the harness.
const dshShellKey = "DSH_SHELL"

// dshSessionIDKey carries the calling session's id.
const dshSessionIDKey = "DSH_SESSION_ID"

// reservedKeys are registry-owned built-ins a contributor can never claim.
var reservedKeys = map[string]struct{}{
	DshHomeEnv:      {},
	dshShellKey:     {},
	dshSessionIDKey: {},
}

// bashEnvKeyPattern constrains contributor-declared key suffixes.
var bashEnvKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// BashEnvVariable is the model-visible metadata for one managed DSH_*
// environment variable.
type BashEnvVariable struct {
	// Description concisely describes the environment fact represented
	// by the variable.
	Description string
}

// BashEnvContributor is a plugin contribution to the managed environment
// of each model shell call. Declared keys make ownership conflicts
// detectable before the first command; Resolve computes only the values
// available for the current execution.
type BashEnvContributor struct {
	// Name is the stable contributor name used in diagnostics and
	// duplicate detection.
	Name string
	// Variables is the complete set of DSH_* keys this contributor may
	// return.
	Variables map[string]BashEnvVariable
	// Resolve computes this contributor's available values for one tool
	// execution. The returned map may contain only keys declared in
	// Variables.
	Resolve func(execution *tools.ToolExecution) map[string]string
}

// BashEnvVariableInfo is an enumerable declaration returned by List.
type BashEnvVariableInfo struct {
	// Contributor owns the variable.
	Contributor string
	// Description concisely describes the environment fact.
	Description string
	// Key is the declared DSH_* environment variable name.
	Key string
}

// ShellEnvRegistry is the ctx.shellEnv registry of trusted, per-execution
// DSH_* variables consumed by the model-facing shell tools. The namespace
// is rebuilt for every model shell call: ambient DSH_* values are discarded
// by the executor, then the registry's current snapshot is injected.
// Built-in shell facts remain owned by the registry itself while plugins
// can register additional, enumerable facts with effect-scoped disposal.
type ShellEnvRegistry struct {
	mu           sync.Mutex
	contributors map[string]BashEnvContributor
	keyOwners    map[string]string
	dshHome      string
	// agents resolves the calling agent from the execution scope (the Go
	// registry pipeline carries a scope key, not the agent object).
	agents func(scope tools.ScopeKey) *agent.Agent
}

// NewShellEnvRegistry creates the registry; dshHome defaults to the
// DSH_HOME environment variable, else ~/.dsh. The agent resolver may be
// nil (DSH_SESSION_ID is then simply absent for every call).
func NewShellEnvRegistry(dshHome string, agents func(scope tools.ScopeKey) *agent.Agent) *ShellEnvRegistry {
	if dshHome == "" {
		dshHome = defaultDshHome()
	}
	return &ShellEnvRegistry{
		contributors: map[string]BashEnvContributor{},
		keyOwners:    map[string]string{},
		dshHome:      dshHome,
		agents:       agents,
	}
}

func defaultDshHome() string {
	if home := strings.TrimSpace(homeEnv()); home != "" {
		return home
	}
	return homePathDefault()
}

// Register adds one environment contributor. Names and keys are unique;
// built-in keys are reserved; every declared key needs a description. The
// returned func unregisters the contribution.
func (r *ShellEnvRegistry) Register(contributor BashEnvContributor) (func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if strings.TrimSpace(contributor.Name) == "" {
		return nil, fmt.Errorf("bash env contributor name must be non-empty")
	}
	if _, has := r.contributors[contributor.Name]; has {
		return nil, fmt.Errorf("bash env contributor %q is already registered", contributor.Name)
	}
	for key, variable := range contributor.Variables {
		if !strings.HasPrefix(key, DshEnvPrefix) || !bashEnvKeyPattern.MatchString(strings.TrimPrefix(key, DshEnvPrefix)) {
			return nil, fmt.Errorf("bash env contributor %q declared invalid key %q", contributor.Name, key)
		}
		if _, isReserved := reservedKeys[key]; isReserved {
			return nil, fmt.Errorf("bash env contributor %q cannot own reserved key %q", contributor.Name, key)
		}
		if strings.TrimSpace(variable.Description) == "" {
			return nil, fmt.Errorf("bash env contributor %q must describe %q", contributor.Name, key)
		}
		if owner, has := r.keyOwners[key]; has {
			return nil, fmt.Errorf("bash env key %q is already owned by contributor %q; contributor %q cannot also own it", key, owner, contributor.Name)
		}
	}
	r.contributors[contributor.Name] = contributor
	for key := range contributor.Variables {
		r.keyOwners[key] = contributor.Name
	}
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.contributors, contributor.Name)
		for key := range contributor.Variables {
			delete(r.keyOwners, key)
		}
	}, nil
}

// Collect builds the trusted DSH_* snapshot for one shell tool execution:
// the built-ins plus current contributions (contributors resolve in name
// order); a contributor returning an undeclared key fails loud.
func (r *ShellEnvRegistry) Collect(execution *tools.ToolExecution) map[string]string {
	r.mu.Lock()
	contributors := make([]BashEnvContributor, 0, len(r.contributors))
	for _, contributor := range r.contributors {
		contributors = append(contributors, contributor)
	}
	r.mu.Unlock()
	sort.Slice(contributors, func(left, right int) bool {
		return contributors[left].Name < contributors[right].Name
	})
	values := map[string]string{
		DshHomeEnv:  r.dshHome,
		dshShellKey: "1",
	}
	if execution != nil && r.agents != nil {
		if caller := r.agents(execution.Agent); caller != nil {
			if id := sessionIDOf(caller); id != "" {
				values[dshSessionIDKey] = id
			}
		}
	}
	for _, contributor := range contributors {
		resolved := contributor.Resolve(execution)
		for key, value := range resolved {
			if _, declared := contributor.Variables[key]; !declared {
				panic(fmt.Sprintf("bash env contributor %q returned undeclared key %q", contributor.Name, key))
			}
			values[key] = value
		}
	}
	return values
}

// List enumerates plugin-contributed variables without executing their
// resolvers, sorted by environment variable name.
func (r *ShellEnvRegistry) List() []BashEnvVariableInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []BashEnvVariableInfo
	for _, contributor := range r.contributors {
		for key, variable := range contributor.Variables {
			out = append(out, BashEnvVariableInfo{
				Contributor: contributor.Name,
				Description: variable.Description,
				Key:         key,
			})
		}
	}
	sort.Slice(out, func(left, right int) bool {
		return out[left].Key < out[right].Key
	})
	return out
}

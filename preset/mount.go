// The standing-mount machinery: per-preset standing compositions agents
// join, single-flight per preset, generation-stamped by the composition
// file.
//
// Port of mount/composeFrom/standingKeyFor/ensureStanding in
// packages/preset/agent-presets/src/index.ts. Go adaptations, each noted
// where it lands:
//   - The standing scope is a child of the host composition context; the
//     standing key is carried to entry plugins through StandingScopeService
//     (the official scopes a context via extend with kScope).
//   - The official leakedServices audit rejects a subtree that published a
//     service into the root realm. Go contexts cannot do that by
//     construction — Provide stores on the exact context, and Get walks UP
//     the ancestor chain only — so the guard is structural rather than
//     runtime.
//   - The official inactiveRows audit rejects rows that never reached a
//     usable state; its Go equivalent is the pending-injection audit over
//     the assembled tree (deferred injections that no Provide ever
//     satisfied), failing loud with the missing services named.
//   - Records are never pruned by fiber observation (Go has no fiber uid):
//     a standing record dies only through regeneration or host teardown.
package preset

import (
	"fmt"
	"strings"
	"sync"

	"dshgo/cordis"
	"dshgo/scope"
)

// StandingScopeService carries the standing scope key on a standing
// context: entry plugins read it to file their registrations into the
// standing layer instead of the global one.
var StandingScopeService = cordis.DefineService[scope.ScopeKey]("agentPresets.standingScope")

// StandingTree is what one assembled standing composition publishes for the
// mount audit and teardown.
type StandingTree interface {
	// PendingInjections reports rows that never reached a usable state:
	// one entry per deferred injection, the services still missing.
	PendingInjections() [][]string
	// Dispose tears the assembled tree down to quiescence.
	Dispose() error
}

// StandingAssembler installs one preset's composition into a standing
// context. The boot layer supplies it: composition text to entries to
// applied plugins.
type StandingAssembler func(standingCtx *cordis.Context, preset AgentPreset) (StandingTree, error)

// ScopeOfAgent reads the scope key one agent context carries; nil for an
// unscoped context. The boot layer supplies it (the factory publishes each
// agent on its own context).
type ScopeOfAgent func(agentCtx *cordis.Context) scope.ScopeKey

// StandingMount is one preset's standing composition currently installed.
type StandingMount struct {
	// PresetID is the preset the standing composition was composed from.
	PresetID string
	// Key is the standing scope key agents are parented to.
	Key scope.ScopeKey
	// Ctx is the standing context the composition was applied into.
	Ctx *cordis.Context
	// Stamp is the composition file identity this generation was mounted
	// from.
	Stamp CompositionStamp

	tree StandingTree
}

// Dispose tears the standing tree down.
func (s *StandingMount) Dispose() error { return s.tree.Dispose() }

// standingGen is one in-flight or settled standing composition: the Go
// equivalent of the official's memoized per-preset promise.
type standingGen struct {
	ready chan struct{}
	mount *StandingMount
	err   error
}

func (g *standingGen) await() (*StandingMount, error) {
	<-g.ready
	return g.mount, g.err
}

func (g *standingGen) finish(mount *StandingMount, err error) {
	g.mount = mount
	g.err = err
	close(g.ready)
}

// MountOptions carries the seams the standing machinery needs from its
// host composition.
type MountOptions struct {
	// Assemble installs one preset's composition into a standing context.
	Assemble StandingAssembler
	// ScopeOf reads an agent context's scope key; nil means unscoped.
	ScopeOf ScopeOfAgent
}

// Mounts owns the standing compositions one deployment supplies.
type Mounts struct {
	roster   *Roster
	host     *cordis.Context
	assemble StandingAssembler
	scopeOf  ScopeOfAgent

	mu       sync.Mutex
	standing map[string]*standingGen
	bindings map[scope.ScopeKey]scope.ScopeParentBinding
}

// NewMounts builds the standing machinery over the roster, rooted at the
// host composition context. Every standing generation disposes with the
// host.
func NewMounts(host *cordis.Context, roster *Roster, options MountOptions) (*Mounts, error) {
	m := &Mounts{
		roster:   roster,
		host:     host,
		assemble: options.Assemble,
		scopeOf:  options.ScopeOf,
		standing: map[string]*standingGen{},
		bindings: map[scope.ScopeKey]scope.ScopeParentBinding{},
	}
	if err := host.Effect(func() (cordis.Disposer, error) {
		return m.teardown, nil
	}); err != nil {
		return nil, err
	}
	return m, nil
}

// teardown disposes every live standing generation at host disposal.
func (m *Mounts) teardown() {
	m.mu.Lock()
	gens := make([]*standingGen, 0, len(m.standing))
	for _, gen := range m.standing {
		gens = append(gens, gen)
	}
	m.standing = map[string]*standingGen{}
	m.mu.Unlock()
	for _, gen := range gens {
		if mounted, err := gen.await(); err == nil && mounted != nil {
			_ = mounted.Dispose()
		}
	}
}

// Resolve resolves one preset id against the roster WITHOUT the
// mountability refusal: the creation transaction reads the row first and
// fails the composition later, exactly like the official resolve/
// standingKeyFor split.
func (m *Mounts) Resolve(id string) (AgentPreset, error) { return m.roster.Resolve(id) }

// List enumerates the roster's presets (the agentPresets/list wire source).
func (m *Mounts) List() ([]AgentPreset, error) { return m.roster.List() }

// Authorable reports whether this deployment has a locally authored preset
// root (the agentPresets/list authorable flag).
func (m *Mounts) Authorable() bool { return m.roster.Authorable() }

// DefaultID reads the deployment's default preset id (empty when unset).
func (m *Mounts) DefaultID() string { return m.roster.DefaultID() }

// Mount installs the preset's standing composition around one agent: the
// agent's scope key binds to the standing key, so every registration the
// composition filed becomes visible down the agent's scope chain. Port of
// mount in index.ts; the unscoped refusal wording is verbatim. An empty id
// mounts the roster's default preset.
func (m *Mounts) Mount(agentCtx *cordis.Context, id string) (AgentPreset, error) {
	if m.scopeOf(agentCtx) == nil {
		return AgentPreset{}, fmt.Errorf("agent-presets: refusing to compose an unscoped context; the scope key is what joins an agent to its preset")
	}
	preset, err := m.roster.ResolveMountable(m.defaulted(id))
	if err != nil {
		return AgentPreset{}, err
	}
	mounted, err := m.ensureStanding(preset)
	if err != nil {
		return AgentPreset{}, err
	}
	agentKey := m.scopeOf(agentCtx)
	// The one bind of this agent's ancestry. The binding is the only
	// re-link authority, held privately so nothing outside this roster can
	// move a composed agent to another preset.
	binding, err := scope.BindParent(agentKey, mounted.Key)
	if err != nil {
		return AgentPreset{}, err
	}
	m.mu.Lock()
	m.bindings[agentKey] = binding
	m.mu.Unlock()
	return preset, nil
}

// ComposeFrom joins one agent to the SAME standing composition another
// already runs on: a bind, not a mount — the parent's exact generation, no
// roster read and no file touch. A parent that joined no preset yields no
// join and no error. Port of composeFrom in index.ts; the refusal wording
// is verbatim.
func (m *Mounts) ComposeFrom(agentCtx, parentCtx *cordis.Context) (string, bool, error) {
	agentKey := m.scopeOf(agentCtx)
	if agentKey == nil {
		return "", false, fmt.Errorf("agent-presets: refusing to compose an unscoped context; the scope key is what joins an agent to its preset")
	}
	standing := m.StandingMountFor(m.scopeOf(parentCtx))
	if standing == nil {
		return "", false, nil
	}
	binding, err := scope.BindParent(agentKey, standing.Key)
	if err != nil {
		return "", false, err
	}
	m.mu.Lock()
	m.bindings[agentKey] = binding
	m.mu.Unlock()
	return standing.PresetID, true, nil
}

// ComposedPreset is the preset one live agent runs on, read from the live
// scope chain rather than from the session. Empty when the agent joined
// none.
func (m *Mounts) ComposedPreset(agentCtx *cordis.Context) (string, bool) {
	standing := m.StandingMountFor(m.scopeOf(agentCtx))
	if standing == nil {
		return "", false
	}
	return standing.PresetID, true
}

// StandingKeyFor is the standing scope key of one preset, for a host
// reader with no agent: ensuring the mount composes plugins but starts no
// agent, no session, and no turn. An empty id resolves the roster default.
func (m *Mounts) StandingKeyFor(id string) (scope.ScopeKey, error) {
	preset, err := m.roster.ResolveMountable(m.defaulted(id))
	if err != nil {
		return nil, err
	}
	mounted, err := m.ensureStanding(preset)
	if err != nil {
		return nil, err
	}
	return mounted.Key, nil
}

// LiveMounts is every standing composition currently installed.
func (m *Mounts) LiveMounts() []*StandingMount {
	m.mu.Lock()
	gens := make([]*standingGen, 0, len(m.standing))
	for _, gen := range m.standing {
		gens = append(gens, gen)
	}
	m.mu.Unlock()
	out := make([]*StandingMount, 0, len(gens))
	for _, gen := range gens {
		select {
		case <-gen.ready:
		default:
			continue
		}
		if gen.mount != nil {
			out = append(out, gen.mount)
		}
	}
	return out
}

// Invalidate drops the standing record for one preset id WITHOUT disposing
// it: a settled mount under a re-created id can only be stale (its preset
// was deleted from disk outside Remove), and agents already joined keep
// their generation regardless — the unlink only stops NEW sessions from
// inheriting a record that no longer names the file on disk.
func (m *Mounts) Invalidate(id string) {
	m.mu.Lock()
	delete(m.standing, id)
	m.mu.Unlock()
}

// defaulted fills an empty id with the roster's default preset.
func (m *Mounts) defaulted(id string) string {
	if id != "" {
		return id
	}
	return m.roster.DefaultID()
}

// ensureStanding resolves (or creates, single-flight) the standing mount
// of one preset. Port of ensureStanding in index.ts, including the
// stamp-driven regeneration with its guarded delete.
func (m *Mounts) ensureStanding(preset AgentPreset) (*StandingMount, error) {
	m.mu.Lock()
	pending, inFlight := m.standing[preset.ID]
	m.mu.Unlock()
	if inFlight {
		mounted, err := pending.await()
		if err != nil {
			return nil, err
		}
		// Files are the only composition editor (authoring is
		// copy/delete), so the stamp is what notices an edit: a changed
		// file starts the next generation here, for this and later
		// sessions. An unreadable stamp serves the current generation —
		// a mount must survive its file disappearing, and failing the
		// session over a stat would not.
		current := StampComposition(preset.Path)
		if current == nil || SameStamp(mounted.Stamp, *current) {
			return mounted, nil
		}
		// TODO: reclaim the superseded generation once the last agent
		// joined to it is gone (the official carries the same TODO; it
		// needs a joined-agent count on StandingMount).
		// Guarded delete: a caller that raced this one may have already
		// started the next generation, and dropping THAT pointer would
		// fork a third.
		m.mu.Lock()
		if m.standing[preset.ID] == pending {
			delete(m.standing, preset.ID)
		}
		m.mu.Unlock()
		return m.ensureStanding(preset)
	}
	gen := &standingGen{ready: make(chan struct{})}
	m.mu.Lock()
	m.standing[preset.ID] = gen
	m.mu.Unlock()
	mounted, err := m.buildStanding(preset)
	gen.finish(mounted, err)
	if err != nil {
		m.mu.Lock()
		delete(m.standing, preset.ID)
		m.mu.Unlock()
		return nil, err
	}
	return mounted, nil
}

// buildStanding composes one standing generation: mint the key, derive the
// standing context, then stamp BEFORE the read and assemble.
func (m *Mounts) buildStanding(preset AgentPreset) (*StandingMount, error) {
	key := scope.NewScopeKey(nil)
	standingCtx := m.host.Child()
	StandingScopeService.Provide(standingCtx, key)
	// Stamped before the file is read: an edit racing the mount makes the
	// stamp stale rather than silently current, so the next session
	// refreshes instead of trusting a composition older than its stamp.
	stamp := StampComposition(preset.Path)
	if stamp == nil {
		_ = standingCtx.Dispose()
		return nil, &PresetMountError{PresetID: preset.ID, Reason: "composition file is unreadable: " + preset.Path}
	}
	tree, err := m.assemble(standingCtx, preset)
	if err != nil {
		_ = standingCtx.Dispose()
		return nil, &PresetMountError{PresetID: preset.ID, Reason: fmt.Sprintf("%v (%s)", err, preset.Path)}
	}
	// The inactiveRows guard, adapted: a row that never reached a usable
	// state is rejected, because its deferred injection sits outside any
	// boot audit otherwise. The missing services are named, one set per
	// stranded row.
	if pending := tree.PendingInjections(); len(pending) > 0 {
		_ = tree.Dispose()
		missing := make([]string, 0, len(pending))
		for _, set := range pending {
			missing = append(missing, strings.Join(set, ","))
		}
		return nil, &PresetMountError{
			PresetID: preset.ID,
			Reason:   fmt.Sprintf("composition rows never activated: missing services %s (%s)", strings.Join(missing, "; "), preset.Path),
		}
	}
	return &StandingMount{PresetID: preset.ID, Key: key, Ctx: standingCtx, Stamp: *stamp, tree: tree}, nil
}

// StandingMountFor locates a live standing mount through one agent already
// joined to it: the agent's key is parented to the standing key, so the
// mount is found by matching that parent. An agent that joined no preset
// has no parent link and resolves to nil.
func (m *Mounts) StandingMountFor(agentKey scope.ScopeKey) *StandingMount {
	if agentKey == nil {
		return nil
	}
	standingKey := scope.ParentOf(agentKey)
	if standingKey == nil {
		return nil
	}
	m.mu.Lock()
	gens := make([]*standingGen, 0, len(m.standing))
	for _, gen := range m.standing {
		gens = append(gens, gen)
	}
	m.mu.Unlock()
	for _, gen := range gens {
		select {
		case <-gen.ready:
		default:
			continue
		}
		if gen.mount != nil && gen.mount.Key == standingKey {
			return gen.mount
		}
	}
	return nil
}

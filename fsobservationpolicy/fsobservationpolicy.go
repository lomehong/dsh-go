// Package fsobservationpolicy ports @deepseek-ai/dsh-fs-observation-policy:
// the event-only filesystem observation policy. It registers no service; a
// per-owner map records every authoritative presence/absence observation,
// the fs/write-intent and fs/edit-intent listeners derive guards from that
// state, and the provider performs the atomic freshness/no-clobber check.
// Without this plugin, tools retain the bare provider's unconditional
// mutation behavior.
package fsobservationpolicy

import (
	"fmt"
	"sync"

	"dshgo/cordis"
	"dshgo/fs"
	"dshgo/tools"
)

// gate is the per-context observed-file state. Entries key on the opaque
// owner (the calling scope key derived from the tool-execution actor), so a
// disposed session's state is simply never addressed again.
type gate struct {
	mu       sync.Mutex
	observed map[any]map[fs.TargetKey]fs.Observation
}

func newGate() *gate {
	return &gate{observed: map[any]map[fs.TargetKey]fs.Observation{}}
}

// ownerOf derives the observed-state owner from the opaque event actor —
// the tool-execution context's calling scope. Nil when no owner can be
// derived (a direct tool call with no agent); such calls read freely but
// cannot satisfy the write/edit prior-observation policy.
func ownerOf(actor any) any {
	if exec, ok := actor.(*tools.ToolRunContext); ok && exec != nil {
		return exec.Agent
	}
	return nil
}

func (g *gate) get(owner any, key fs.TargetKey) (fs.Observation, bool) {
	byTarget, ok := g.observed[owner]
	if !ok {
		return fs.Observation{}, false
	}
	observation, ok := byTarget[key]
	return observation, ok
}

func (g *gate) set(owner any, key fs.TargetKey, observation fs.Observation) {
	byTarget, ok := g.observed[owner]
	if !ok {
		byTarget = map[fs.TargetKey]fs.Observation{}
		g.observed[owner] = byTarget
	}
	byTarget[key] = observation
}

// clear drops all recorded state (HMR safety / disposal): replacing the map
// makes the release observable and immediate for tests.
func (g *gate) clear() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.observed = map[any]map[fs.TargetKey]fs.Observation{}
}

// writeIntent decides the write intent: unseen or confirmed absent ⇒
// createIfAbsent; confirmed present ⇒ replaceIfVersion at the observed
// version. An ownerless actor reads the same unseen path.
func (g *gate) writeIntent(target fs.Target, actor any) *fs.WriteIntent {
	owner := ownerOf(actor)
	g.mu.Lock()
	defer g.mu.Unlock()
	var prior fs.Observation
	if owner != nil {
		prior, _ = g.get(owner, target.Key)
	}
	if prior.Present {
		return &fs.WriteIntent{Kind: fs.IntentReplaceIfVersion, Version: prior.Version}
	}
	return &fs.WriteIntent{Kind: fs.IntentCreateIfAbsent}
}

// editIntent decides the edit version guard: unseen rejects with
// FS_NOT_OBSERVED, confirmed absence rejects with FS_NOT_FOUND, and
// presence supplies the observed version as the CAS basis.
func (g *gate) editIntent(target fs.Target, actor any) (*fs.Version, error) {
	owner := ownerOf(actor)
	g.mu.Lock()
	prior, seen := fs.Observation{}, false
	if owner != nil {
		prior, seen = g.get(owner, target.Key)
	}
	g.mu.Unlock()
	if owner == nil || !seen {
		return nil, fs.NewError(fs.CodeNotObserved, fmt.Sprintf("edit requires reading %q first", target.DisplayPath), nil)
	}
	if !prior.Present {
		return nil, fs.NewError(fs.CodeNotFound, fmt.Sprintf("cannot edit %q: not found", target.DisplayPath), nil)
	}
	version := prior.Version
	return &version, nil
}

// observe records an authoritative present or absent observation for this
// owner and target.
func (g *gate) observe(target fs.Target, observation fs.Observation, actor any) {
	owner := ownerOf(actor)
	if owner == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.set(owner, target.Key, observation)
}

// Apply registers the three fs/* listeners on one plugin context. No
// services are injected — the gate operates only on its own state. The
// intent listeners occupy the single decision slot (they do not call next);
// fs/observed stays synchronous and non-throwing because successful
// mutations have already committed.
func Apply(ctx *cordis.Context) func() {
	g := newGate()
	undoIntent := ctx.On(fs.EventWriteIntent, func(value any, next func(any) any) any {
		if event, ok := value.(fs.WriteIntentEvent); ok {
			return g.writeIntent(event.Target, event.Actor)
		}
		return nil
	})
	undoEdit := ctx.On(fs.EventEditIntent, func(value any, next func(any) any) any {
		if event, ok := value.(fs.EditIntentEvent); ok {
			version, err := g.editIntent(event.Target, event.Actor)
			if err != nil {
				return err
			}
			return version
		}
		return nil
	})
	undoObserved := ctx.On(fs.EventObserved, func(value any, next func(any) any) any {
		if event, ok := value.(fs.ObservedEvent); ok {
			g.observe(event.Target, event.Observation, event.Actor)
		}
		return nil
	})
	return func() {
		undoIntent()
		undoEdit()
		undoObserved()
		g.clear()
	}
}

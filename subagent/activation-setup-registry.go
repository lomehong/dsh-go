package subagent

import (
	"fmt"
	"sync"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/llm"
)

// ContinuableSetupContribution is one deployment capability installed into a
// continuable child's unpublished creation context. It composes
// synchronously before publication and returns the disposer for exactly
// that installation. A contribution grants a child-scoped capability without
// teaching the continuation manager which capabilities exist.
type ContinuableSetupContribution = func(childCtx *cordis.Context) func()

// installation is one contribution installed into one child context.
type installation struct {
	registration *setupRegistration
	childCtx     *cordis.Context
	dispose      func()
	released     bool
	// transaction is the child's provisioning batch, present until the child
	// reaches residency.
	transaction *setupTransaction
}

// setupRegistration is one contribution's live registration.
type setupRegistration struct {
	contribution  ContinuableSetupContribution
	removed       bool
	installations map[*installation]struct{}
}

// setupTransaction is one child's provisioning batch.
type setupTransaction struct {
	installations []*installation
	invalidated   bool
}

// ActivationSetupRegistry owns continuable-child setup registrations,
// installations, rollback, child cleanup, and immediate live revocation.
// The continuation manager owns residency; this registry owns the join
// between plugin lifetime, unpublished setup, and Activation disposal, so no
// installation outlives either owner and no removed contribution can be
// installed after revocation reports completion.
type ActivationSetupRegistry struct {
	mu            sync.Mutex
	registrations map[*setupRegistration]struct{}
	byChild       map[*cordis.Context]map[*installation]struct{}
}

// NewActivationSetupRegistry builds one empty registry.
func NewActivationSetupRegistry() *ActivationSetupRegistry {
	return &ActivationSetupRegistry{
		registrations: map[*setupRegistration]struct{}{},
		byChild:       map[*cordis.Context]map[*installation]struct{}{},
	}
}

// Register adds one contribution and returns an idempotent registration
// undo. It fails after attempting every installation when any disposer
// fails.
func (r *ActivationSetupRegistry) Register(contribution ContinuableSetupContribution) (undo func(), err error) {
	registration := &setupRegistration{
		contribution:  contribution,
		installations: map[*installation]struct{}{},
	}
	r.mu.Lock()
	r.registrations[registration] = struct{}{}
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		if registration.removed {
			r.mu.Unlock()
			return
		}
		// Close before disposal so a snapshotted apply() cannot install
		// after revocation reports completion.
		registration.removed = true
		delete(r.registrations, registration)
		live := make([]*installation, 0, len(registration.installations))
		for candidate := range registration.installations {
			live = append(live, candidate)
		}
		r.mu.Unlock()
		r.releaseAll(live, "contribution removal")
	}, nil
}

// Apply installs every live contribution into one unpublished child context
// and returns the provisioning commit consumed at Agent publication. An
// installer failure keeps that failure authoritative after rolling every
// installation in the batch back.
func (r *ActivationSetupRegistry) Apply(childCtx *cordis.Context) (commit agent.AgentSetupCommit, err error) {
	state := &setupTransaction{}
	defer func() {
		if rec := recover(); rec != nil {
			r.releaseAll(state.installations, "setup rollback")
			err = fmt.Errorf("continuable-subagent setup contribution panicked: %v", rec)
			commit = agent.AgentSetupCommit{}
		}
	}()
	r.mu.Lock()
	live := make([]*setupRegistration, 0, len(r.registrations))
	for registration := range r.registrations {
		live = append(live, registration)
	}
	r.mu.Unlock()
	for _, registration := range live {
		// Only a synchronous re-entrant revocation of an already-snapshotted
		// registration reaches this guard.
		r.mu.Lock()
		removed := registration.removed
		r.mu.Unlock()
		if removed {
			continue
		}
		dispose := registration.contribution(childCtx)
		created := &installation{
			registration: registration,
			childCtx:     childCtx,
			dispose:      dispose,
			transaction:  state,
		}
		r.mu.Lock()
		registration.installations[created] = struct{}{}
		state.installations = append(state.installations, created)
		indexed := r.byChild[childCtx]
		if indexed == nil {
			indexed = map[*installation]struct{}{}
			r.byChild[childCtx] = indexed
		}
		indexed[created] = struct{}{}
		escaped := registration.removed
		r.mu.Unlock()
		// An installer may revoke itself before its installation record
		// exists. Dispose that escaped record and invalidate the
		// provisioning batch.
		if escaped {
			r.release(created)
		}
	}
	if err := childCtx.Effect(func() (cordis.Disposer, error) {
		return func() { r.releaseChild(childCtx) }, nil
	}); err != nil {
		r.releaseAll(state.installations, "setup rollback")
		return agent.AgentSetupCommit{}, err
	}
	return agent.AgentSetupCommit{Commit: func() error {
		r.mu.Lock()
		invalidated := state.invalidated
		r.mu.Unlock()
		if invalidated {
			return newSubagentError(
				"a continuable-subagent setup contribution was revoked while this child was being built; "+
					"the child was not established",
				CodeActivationSetupRevoked, nil)
		}
		r.mu.Lock()
		for _, created := range state.installations {
			created.transaction = nil
		}
		r.mu.Unlock()
		return nil
	}}, nil
}

// releaseChild releases every remaining installation owned by one disposed
// child scope.
func (r *ActivationSetupRegistry) releaseChild(childCtx *cordis.Context) {
	r.mu.Lock()
	indexed := r.byChild[childCtx]
	live := make([]*installation, 0, len(indexed))
	for candidate := range indexed {
		live = append(live, candidate)
	}
	r.mu.Unlock()
	r.releaseAll(live, "child scope disposal")
}

// releaseAll releases a batch completely before reporting disposer failures.
func (r *ActivationSetupRegistry) releaseAll(installations []*installation, during string) {
	var failures []error
	for _, candidate := range installations {
		if err := r.releaseContained(candidate); err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) == 0 {
		return
	}
	rendered := ""
	for index, failure := range failures {
		if index > 0 {
			rendered += "; "
		}
		rendered += llm.ErrorChain(failure)
	}
	panic(newSubagentError(
		fmt.Sprintf("continuable-subagent setup %s failed to release %d installation(s): %s",
			during, len(failures), rendered),
		CodeActivationSetupReleaseFailed, nil))
}

// releaseContained runs one release with panic containment so a failing
// disposer cannot starve the remaining releases in the batch.
func (r *ActivationSetupRegistry) releaseContained(target *installation) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			if failure, ok := rec.(error); ok {
				err = failure
				return
			}
			err = fmt.Errorf("installation disposer failed: %v", rec)
		}
	}()
	return r.release(target)
}

// release drops one installation from both indices and disposes it exactly
// once. Matching the source, a disposer failure surfaces as a panic so the
// releaseAll aggregator can collect it.
func (r *ActivationSetupRegistry) release(target *installation) error {
	r.mu.Lock()
	if target.released {
		r.mu.Unlock()
		return nil
	}
	target.released = true
	delete(target.registration.installations, target)
	if indexed := r.byChild[target.childCtx]; indexed != nil {
		delete(indexed, target)
		if len(indexed) == 0 {
			delete(r.byChild, target.childCtx)
		}
	}
	if target.transaction != nil {
		target.transaction.invalidated = true
	}
	r.mu.Unlock()
	if target.dispose == nil {
		return nil
	}
	// Keep the panic contract: a failing disposer aborts the caller exactly
	// like the source's thrown error.
	target.dispose()
	return nil
}

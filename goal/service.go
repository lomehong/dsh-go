package goal

import (
	"crypto/rand"
	"fmt"
	"strings"
	"sync"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/session/projection"
)

// defaultMaxGoalRoundsDefault is the deployment round cap applied when a
// create request omits its own and the config carries none (official zod
// default 256).
const defaultMaxGoalRoundsDefault = 256

// projectionsService is the catalog service name of the session projection
// registry (boot.ServiceProjections; named literally here to avoid an
// import cycle through boot).
const projectionsService = "projections"

// Config carries the deployment defaults for goal creation.
type Config struct {
	// DefaultMaxGoalRounds is the total rounds used when a create request
	// omits its own cap; nil applies 256.
	DefaultMaxGoalRounds *int64
}

// goalCache is the process-local cache plus the activation intent crossing
// the synchronous append boundary.
type goalCache struct {
	state             *FoldState
	activation        GoalActivation
	observedSeq       int64
	pendingActivation *pendingActivation
}

type pendingActivation struct {
	seq        int64
	activation GoalActivation
}

// Service is the goal service (official `ctx.goals`) backed exclusively by
// the owning session log.
type Service struct {
	agents               *agent.AgentRegistry
	defaultMaxGoalRounds int64

	mu     sync.Mutex
	caches map[*session.Session]*goalCache

	disposers []func()
}

// NewService validates the config, subscribes the session-start disarm
// edge, and optionally attaches the `goal` projection unit when the
// projection registry is composed (headless assemblies stay unaffected).
func NewService(ctx *cordis.Context, agents *agent.AgentRegistry, config Config) (*Service, error) {
	defaultRounds := int64(defaultMaxGoalRoundsDefault)
	if config.DefaultMaxGoalRounds != nil {
		defaultRounds = *config.DefaultMaxGoalRounds
	}
	resolved, err := resolveMaxGoalRounds(defaultRounds)
	if err != nil {
		return nil, err
	}
	session.EnsureEventTypes(EventChange)
	service := &Service{
		agents:               agents,
		defaultMaxGoalRounds: resolved,
		caches:               map[*session.Session]*goalCache{},
	}
	service.disposers = append(service.disposers, agents.Events().OnEmit(
		agent.EventAgentSessionStart, nil, func(payload any) error {
			start, ok := payload.(agent.AgentSessionStartPayload)
			if !ok {
				return nil
			}
			// A session-start edge disarms automatic continuation; the
			// durable phase is untouched.
			service.mu.Lock()
			defer service.mu.Unlock()
			cache, err := service.cacheLocked(start.Agent.Session)
			if err != nil {
				return err
			}
			cache.activation = ActivationDisarmed
			return nil
		}))
	if ctx != nil {
		// The unit child activates only when a projection registry is
		// composed (the official ctx.inject(['sessionProjections']) seam).
		if err := ctx.Inject([]string{projectionsService}, func(c *cordis.Context) error {
			registry, ok := c.Get(projectionsService).(*projection.Registry)
			if !ok {
				return fmt.Errorf("goal: service %q is not a projection registry", projectionsService)
			}
			dispose, err := registry.Register(GoalUnit().Definition())
			if err != nil {
				return err
			}
			service.mu.Lock()
			service.disposers = append(service.disposers, dispose)
			service.mu.Unlock()
			return nil
		}); err != nil {
			service.Dispose()
			return nil, err
		}
	}
	return service, nil
}

// Dispose withdraws the bus subscription and the projection registration.
func (s *Service) Dispose() {
	s.mu.Lock()
	disposers := s.disposers
	s.disposers = nil
	s.mu.Unlock()
	for _, dispose := range disposers {
		dispose()
	}
}

// newGoalError builds one goal boundary error (the official GoalError over
// the harness error base).
func newGoalError(message string, code GoalErrorCode) error {
	return llm.NewError(code, message, nil)
}

// resolveMaxGoalRounds validates a caller-visible positive safe-integer
// round cap.
func resolveMaxGoalRounds(value int64) (int64, error) {
	if value < 1 || value > maxSafeInteger {
		return 0, newGoalError("maxGoalRounds must be a positive safe integer", CodeInvalidMaxRounds)
	}
	return value, nil
}

// resolveObjective validates and normalizes an objective at the domain
// boundary.
func resolveObjective(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", newGoalError("goal objective must be a non-empty string", CodeInvalidObjective)
	}
	return strings.TrimSpace(value), nil
}

// resolveBlockReason validates and detaches one policy-owned blocker
// explanation.
func resolveBlockReason(reason GoalBlockReason) (*GoalBlockReason, error) {
	if !kebabCodePattern.MatchString(reason.Code) || strings.TrimSpace(reason.Message) == "" {
		return nil, newGoalError(
			"goal block reason requires a lower-kebab-case code and a non-empty message",
			CodeInvalidBlockReason)
	}
	return &GoalBlockReason{Code: reason.Code, Message: strings.TrimSpace(reason.Message)}, nil
}

// newGoalID mints one fresh goal identity (official `goal-<uuid>`).
func newGoalID() GoalID {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return GoalID(fmt.Sprintf("goal-%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]))
}

// Get reads the current goal for one exact live agent: a fresh view or nil
// when no goal is current.
func (s *Service) Get(a *agent.Agent) (*GoalView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.assertLive(a); err != nil {
		return nil, err
	}
	cache, err := s.cacheLocked(a.Session)
	if err != nil {
		return nil, err
	}
	if err := s.syncLocked(a.Session, cache); err != nil {
		return nil, err
	}
	return s.view(cache)
}

// Disarm removes process-local continuation authority without changing
// durable goal phase or revision. Lifecycle owners use this before
// unloading a driver; a later human-authorized Resume records the new
// activation edge.
func (s *Service) Disarm(a *agent.Agent) (*GoalView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.assertLive(a); err != nil {
		return nil, err
	}
	cache, err := s.cacheLocked(a.Session)
	if err != nil {
		return nil, err
	}
	if err := s.syncLocked(a.Session, cache); err != nil {
		return nil, err
	}
	cache.activation = ActivationDisarmed
	return s.view(cache)
}

// Create creates and arms a goal. A completed goal may be replaced; every
// other current phase must be cleared or resumed instead.
func (s *Service) Create(a *agent.Agent, request CreateGoalRequest) (*GoalView, error) {
	objective, err := resolveObjective(request.Objective)
	if err != nil {
		return nil, err
	}
	maxGoalRounds := s.defaultMaxGoalRounds
	if request.MaxGoalRounds != nil {
		maxGoalRounds = *request.MaxGoalRounds
	}
	maxGoalRounds, err = resolveMaxGoalRounds(maxGoalRounds)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	cache, err := s.prepareMutationLocked(a)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if current := cache.state.Goal; current != nil && current.Phase != PhaseComplete {
		s.mu.Unlock()
		return nil, newGoalError(
			fmt.Sprintf("goal %q already exists with phase %q", current.ID, current.Phase),
			CodeAlreadyExists)
	}
	now := nowMilliseconds()
	goal := GoalSnapshot{
		ID: newGoalID(), Revision: 1, Objective: objective,
		Phase: PhaseActive, MaxGoalRounds: maxGoalRounds,
	}
	view, err := s.commitSnapshotLocked(a, cache, OperationCreate, goal, 0, now, now, ActivationArmed)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	s.emitChanged(a, OperationCreate, view)
	return view, nil
}

// Edit edits objective and/or round cap without changing phase.
func (s *Service) Edit(a *agent.Agent, ref GoalRef, request EditGoalRequest) (*GoalView, error) {
	s.mu.Lock()
	cache, err := s.prepareMutationLocked(a)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	current, err := s.expectCurrent(cache, ref)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if request.Objective == nil && request.MaxGoalRounds == nil {
		s.mu.Unlock()
		return nil, newGoalError("goal edit requires objective and/or maxGoalRounds", CodeInvalidEdit)
	}
	goal := *current
	goal.Revision = current.Revision + 1
	if request.Objective != nil {
		objective, err := resolveObjective(*request.Objective)
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		goal.Objective = objective
	}
	if request.MaxGoalRounds != nil {
		maxGoalRounds, err := resolveMaxGoalRounds(*request.MaxGoalRounds)
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		goal.MaxGoalRounds = maxGoalRounds
	}
	view, err := s.commitCurrentLocked(a, cache, OperationEdit, goal, cache.activation)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	s.emitChanged(a, OperationEdit, view)
	return view, nil
}

// Pause pauses an active goal and disarms automatic continuation.
func (s *Service) Pause(a *agent.Agent, ref GoalRef) (*GoalView, error) {
	return s.transition(a, ref, OperationPause, []GoalPhase{PhaseActive}, PhasePaused, ActivationDisarmed)
}

// Resume resumes and arms a stopped goal, or rearms an active goal after a
// session-start edge, while its round budget still has capacity.
func (s *Service) Resume(a *agent.Agent, ref GoalRef) (*GoalView, error) {
	s.mu.Lock()
	cache, err := s.prepareMutationLocked(a)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	current, err := s.expectCurrent(cache, ref)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	resumable := []GoalPhase{PhaseActive, PhasePaused, PhaseBlocked}
	if !phaseIn(current.Phase, resumable) {
		s.mu.Unlock()
		return nil, s.transitionError(current, OperationResume, resumable)
	}
	if current.Phase == PhaseActive && cache.activation == ActivationArmed {
		s.mu.Unlock()
		return nil, newGoalError(
			fmt.Sprintf("goal %q is already active and armed", current.ID), CodeInvalidTransition)
	}
	if cache.state.RoundsStarted >= current.MaxGoalRounds {
		s.mu.Unlock()
		return nil, newGoalError(fmt.Sprintf(
			"goal %q exhausted %d goal rounds; increase maxGoalRounds before resuming",
			current.ID, current.MaxGoalRounds), CodeInvalidTransition)
	}
	view, err := s.commitCurrentLocked(a, cache, OperationResume, withPhase(current, PhaseActive), ActivationArmed)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	s.emitChanged(a, OperationResume, view)
	return view, nil
}

// Complete marks a current non-complete goal complete and disarms it.
func (s *Service) Complete(a *agent.Agent, ref GoalRef) (*GoalView, error) {
	return s.transition(a, ref, OperationComplete,
		[]GoalPhase{PhaseActive, PhasePaused, PhaseBlocked}, PhaseComplete, ActivationDisarmed)
}

// Block marks an active goal blocked and disarms it.
func (s *Service) Block(a *agent.Agent, ref GoalRef, reason GoalBlockReason) (*GoalView, error) {
	resolved, err := resolveBlockReason(reason)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	cache, err := s.prepareMutationLocked(a)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	current, err := s.expectCurrent(cache, ref)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if current.Phase != PhaseActive {
		s.mu.Unlock()
		return nil, s.transitionError(current, OperationBlock, []GoalPhase{PhaseActive})
	}
	goal := withPhase(current, PhaseBlocked)
	goal.BlockedReason = resolved
	view, err := s.commitCurrentLocked(a, cache, OperationBlock, goal, ActivationDisarmed)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	s.emitChanged(a, OperationBlock, view)
	return view, nil
}

// Clear clears the current goal while retaining a durable tombstone and
// history. It returns the tombstone ref whose revision is one past the
// cleared snapshot.
func (s *Service) Clear(a *agent.Agent, ref GoalRef) (*GoalRef, error) {
	s.mu.Lock()
	cache, err := s.prepareMutationLocked(a)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	current, err := s.expectCurrent(cache, ref)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	tombstone := GoalRef{ID: current.ID, Revision: current.Revision + 1}
	change := newClearChange(tombstone, s.nextMutationTime(cache))
	if err := s.commitLocked(a, cache, change, ActivationDisarmed); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	view, err := s.view(cache)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	s.emitChangedClear(a, tombstone, view)
	cleared := tombstone
	return &cleared, nil
}

// CreateResult creates one goal through the remote boundary shape,
// acknowledging with the created identity only.
func (s *Service) CreateResult(a *agent.Agent, request CreateGoalRequest) (CreateGoalResult, error) {
	view, err := s.Create(a, request)
	if err != nil {
		return CreateGoalResult{}, err
	}
	return CreateGoalResult{Ref: GoalRef{ID: view.ID, Revision: view.Revision}}, nil
}

// transition runs the shared validated phase transition.
func (s *Service) transition(
	a *agent.Agent, ref GoalRef, operation GoalOperation,
	allowed []GoalPhase, phase GoalPhase, activation GoalActivation,
) (*GoalView, error) {
	s.mu.Lock()
	cache, err := s.prepareMutationLocked(a)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	current, err := s.expectCurrent(cache, ref)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if !phaseIn(current.Phase, allowed) {
		s.mu.Unlock()
		return nil, s.transitionError(current, operation, allowed)
	}
	view, err := s.commitCurrentLocked(a, cache, operation, withPhase(current, phase), activation)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	s.emitChanged(a, operation, view)
	return view, nil
}

// prepareMutationLocked resolves and validates the cache used by a
// mutation. The caller holds s.mu.
func (s *Service) prepareMutationLocked(a *agent.Agent) (*goalCache, error) {
	if err := s.assertLive(a); err != nil {
		return nil, err
	}
	cache, err := s.cacheLocked(a.Session)
	if err != nil {
		return nil, err
	}
	if err := s.syncLocked(a.Session, cache); err != nil {
		return nil, err
	}
	return cache, nil
}

// expectCurrent rejects stale or missing current-state refs.
func (s *Service) expectCurrent(cache *goalCache, ref GoalRef) (*GoalSnapshot, error) {
	current := cache.state.Goal
	if current == nil {
		return nil, newGoalError("no current goal", CodeNotFound)
	}
	if ref.ID != current.ID || ref.Revision != current.Revision {
		return nil, newGoalError(fmt.Sprintf(
			"stale goal ref %q revision %d; current is %q revision %d",
			ref.ID, ref.Revision, current.ID, current.Revision), CodeStaleRevision)
	}
	return current, nil
}

// assertLive enforces exact live-agent identity rather than trusting a
// matching id.
func (s *Service) assertLive(a *agent.Agent) error {
	if s.agents.Get(a.ID) != a {
		return newGoalError(fmt.Sprintf("agent %q is not live in this registry", a.ID), CodeAgentNotLive)
	}
	return nil
}

// cacheLocked returns the per-session cache, folding a seed once with
// activation disarmed. The caller holds s.mu.
func (s *Service) cacheLocked(sess *session.Session) (*goalCache, error) {
	if cache, ok := s.caches[sess]; ok {
		return cache, nil
	}
	state := EmptyFoldState()
	for _, event := range sess.Events() {
		if err := ApplyGoalEvent(state, event); err != nil {
			return nil, err
		}
	}
	cache := &goalCache{state: state, activation: ActivationDisarmed, observedSeq: int64(sess.Seq())}
	s.caches[sess] = cache
	return cache, nil
}

// syncLocked incrementally observes durable events and reconciles local
// activation intent. The caller holds s.mu.
func (s *Service) syncLocked(sess *session.Session, cache *goalCache) error {
	events := sess.Events()
	for _, event := range events[cache.observedSeq:] {
		if err := ApplyGoalEvent(cache.state, event); err != nil {
			return err
		}
		if event.Type == EventChange {
			if pending := cache.pendingActivation; pending != nil && pending.seq == event.Seq {
				cache.activation = pending.activation
			} else {
				cache.activation = ActivationDisarmed
			}
		}
		cache.observedSeq++
	}
	return nil
}

// withPhase builds a new revision with one replacement phase.
func withPhase(current *GoalSnapshot, phase GoalPhase) GoalSnapshot {
	return GoalSnapshot{
		ID: current.ID, Revision: current.Revision + 1,
		Objective: current.Objective, Phase: phase, MaxGoalRounds: current.MaxGoalRounds,
	}
}

// transitionError renders a stable invalid-transition error.
func (s *Service) transitionError(current *GoalSnapshot, operation GoalOperation, allowed []GoalPhase) error {
	words := make([]string, len(allowed))
	for i, phase := range allowed {
		words[i] = string(phase)
	}
	return newGoalError(fmt.Sprintf(
		"cannot %s goal %q from phase %q; expected %s",
		operation, current.ID, current.Phase, strings.Join(words, " or ")), CodeInvalidTransition)
}

// commitCurrentLocked commits a mutation that retains the current goal's
// derived counters/times. The caller holds s.mu.
func (s *Service) commitCurrentLocked(
	a *agent.Agent, cache *goalCache, operation GoalOperation,
	goal GoalSnapshot, activation GoalActivation,
) (*GoalView, error) {
	if cache.state.CreatedAt == nil {
		// Strict replay and every snapshot commit set createdAt whenever a
		// current goal exists.
		return nil, fmt.Errorf("current goal cache lacks createdAt")
	}
	return s.commitSnapshotLocked(a, cache, operation, goal,
		cache.state.RoundsStarted, *cache.state.CreatedAt, s.nextMutationTime(cache), activation)
}

// nextMutationTime clamps a current goal's next timestamp across backward
// wall-clock movement.
func (s *Service) nextMutationTime(cache *goalCache) int64 {
	if cache.state.UpdatedAt == nil {
		// Strict replay and every snapshot commit set updatedAt whenever a
		// current goal exists; the official port panics here too (v8-ignored
		// unreachable), but a returned zero would corrupt the stream, so the
		// caller contract is enforced at the commit path instead.
		return nowMilliseconds()
	}
	now := nowMilliseconds()
	if now < *cache.state.UpdatedAt {
		return *cache.state.UpdatedAt
	}
	return now
}

// commitSnapshotLocked builds and commits one full-snapshot mutation. The
// caller holds s.mu.
func (s *Service) commitSnapshotLocked(
	a *agent.Agent, cache *goalCache, operation GoalOperation, goal GoalSnapshot,
	roundsStarted, createdAt, updatedAt int64, activation GoalActivation,
) (*GoalView, error) {
	change := newSnapshotChange(operation, goal, roundsStarted, createdAt, updatedAt)
	if err := s.commitLocked(a, cache, change, activation); err != nil {
		return nil, err
	}
	view, err := s.view(cache)
	if err != nil {
		return nil, err
	}
	if view == nil {
		// The durable goal event installs the snapshot before this read.
		return nil, fmt.Errorf("snapshot commit cleared the goal unexpectedly")
	}
	return view, nil
}

// commitLocked commits one mutation into the goal log, cache, and live
// state. The caller holds s.mu and emits the notification after releasing
// it (listeners may reenter the service).
func (s *Service) commitLocked(a *agent.Agent, cache *goalCache, change ChangeMeta, activation GoalActivation) error {
	cache.pendingActivation = &pendingActivation{seq: int64(a.Session.Seq()), activation: activation}
	defer func() { cache.pendingActivation = nil }()
	if _, err := a.Session.Append(EventChange, change, nil); err != nil {
		return err
	}
	return s.syncLocked(a.Session, cache)
}

// view builds a detached current view.
func (s *Service) view(cache *goalCache) (*GoalView, error) {
	goal := cache.state.Goal
	if goal == nil {
		return nil, nil //nolint:nilnil // nil IS the no-current-goal value
	}
	if cache.state.CreatedAt == nil || cache.state.UpdatedAt == nil {
		// Strict replay and snapshot commits establish both timestamps with
		// every current goal.
		return nil, fmt.Errorf("goal %q cache lacks timestamps", goal.ID)
	}
	return &GoalView{
		GoalSnapshot:  *goal,
		RoundsStarted: cache.state.RoundsStarted,
		CreatedAt:     *cache.state.CreatedAt,
		UpdatedAt:     *cache.state.UpdatedAt,
		Activation:    cache.activation,
	}, nil
}

// emitChanged publishes the accepted snapshot mutation to the agent-scoped
// `goal/changed` listeners.
func (s *Service) emitChanged(a *agent.Agent, operation GoalOperation, view *GoalView) {
	a.Events().Emit(EventChanged, a.Scope, ChangedPayload{
		Agent: a,
		Change: GoalChanged{
			Operation: operation,
			Ref:       GoalRef{ID: view.ID, Revision: view.Revision},
			Goal:      view,
		},
	})
}

// emitChangedClear publishes the accepted clear tombstone.
func (s *Service) emitChangedClear(a *agent.Agent, tombstone GoalRef, view *GoalView) {
	a.Events().Emit(EventChanged, a.Scope, ChangedPayload{
		Agent: a,
		Change: GoalChanged{
			Operation: OperationClear,
			Ref:       tombstone,
			Goal:      view,
		},
	})
}

func phaseIn(phase GoalPhase, allowed []GoalPhase) bool {
	for _, candidate := range allowed {
		if phase == candidate {
			return true
		}
	}
	return false
}

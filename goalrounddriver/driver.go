// Same-session goal-round driver over public agent, session, and goal
// services (official dsh-goal-round-driver index.ts).
//
// Go adaptations of the JS async model, all preserving the official
// observable behavior:
//   - The serialized per-agent run promise becomes one goroutine per agent
//     with a request flag; triggers coalesce onto it exactly like the
//     official requestDrive/retire pair.
//   - ctx.fiber.state === ACTIVE becomes the service's closed flag: after
//     teardown begins, nothing may drive.
//   - isDeepStrictEqual becomes JSON-canonicalized structural equality.
//   - Throwing through the pre-step waterfall boundary is impossible in Go;
//     a failed prompt-rejected block is warned instead of escalating into
//     the turn (recorded deviation).
package goalrounddriver

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/goal"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/session/projection"
)

// Round-attempt phases: one queued, claimed, or admitted goal message
// retained until whole-agent quiescence.
const (
	attemptQueued   = "queued"
	attemptClaimed  = "claimed"
	attemptAdmitted = "admitted"
)

// roundAttempt is one queued, claimed, or admitted goal message retained
// until whole-agent quiescence.
type roundAttempt struct {
	goalID    goal.GoalID
	revision  int64
	round     int64
	messageID llm.MessageID
	content   []llm.ContentBlock
	phase     string
	cancelled bool
	stale     bool
}

// driverState is the serialized process-local scheduling state for one exact
// Agent lifecycle.
type driverState struct {
	agent           *agent.Agent
	attempt         *roundAttempt
	competingQueued bool
	needsCheckpoint bool
	requested       bool
	running         bool
	done            chan struct{}
	stopping        bool
}

// SessionFlusher persists one exact session before the driver reserves the
// next round (the official sessions service flush face).
type SessionFlusher interface {
	FlushSession(sess *session.Session) error
}

// Config carries the optional deployment faces.
type Config struct {
	// Logger receives the driver's containment warnings; nil drops them.
	Logger cordis.Logger
	// Flusher is the durability checkpoint face; nil falls back to the
	// session store's drain.
	Flusher SessionFlusher
}

// Service installs automatic same-session continuation and its race fences.
type Service struct {
	agents   *agent.AgentRegistry
	goals    *goal.Service
	sessions *session.Store
	config   Config

	mu     sync.Mutex
	closed bool
	states map[*agent.Agent]*driverState

	disposeOnce sync.Once
	disposers   []func()
}

// New installs the driver's composite effect onto the context: listeners,
// the startup disarm sweep, and the quiescent teardown.
func New(ctx *cordis.Context, agents *agent.AgentRegistry, goals *goal.Service,
	sessions *session.Store, config Config,
) (*Service, error) {
	service := &Service{
		agents:   agents,
		goals:    goals,
		sessions: sessions,
		config:   config,
		states:   map[*agent.Agent]*driverState{},
	}
	if config.Logger == nil {
		service.config.Logger = cordis.Discard{}
	}
	err := ctx.Effect(func() (cordis.Disposer, error) {
		service.disposers = append(service.register(agents), ctx.On("session/event", func(value any, next func(any) any) any {
			if payload, ok := value.(*projection.SessionEventPayload); ok {
				service.OnSessionEvent(payload.Session, payload.Event)
			}
			return next(value)
		}))
		// Loading a lifecycle driver over existing agents never inherits
		// hidden automatic authority from an earlier producer instance.
		for _, existing := range agents.List() {
			service.disarm(service.stateFor(existing))
		}
		return service.Dispose, nil
	})
	if err != nil {
		return nil, err
	}
	return service, nil
}

// Dispose removes the listeners, then settles every driver before the state
// table clears. Idempotent: the context teardown and a direct call converge.
func (s *Service) Dispose() {
	s.disposeOnce.Do(func() {
		s.stop()
		for _, dispose := range s.disposers {
			dispose()
		}
	})
}

// --- source comparisons ------------------------------------------------------

// isGoalRoundSource reports whether a source identifies an automatic,
// positive-numbered goal round.
func isGoalRoundSource(source llm.MessageSource) bool {
	return source.Kind == llm.SourceGoal && source.GoalRound > 0
}

// sameRound compares a source to one reserved identity.
func sameRound(source llm.MessageSource, attempt *roundAttempt) bool {
	return source.GoalID == string(attempt.goalID) &&
		source.GoalRevision == attempt.revision &&
		source.GoalRound == attempt.round
}

// sameQueued compares the complete queued record to the driver's
// reservation.
func sameQueued(content []llm.ContentBlock, source llm.MessageSource, attempt *roundAttempt) bool {
	return isGoalRoundSource(source) && sameRound(source, attempt) && contentEqual(content, attempt.content)
}

// contentEqual is the isDeepStrictEqual adaptation: JSON-canonicalized
// structural equality over the wire shape.
func contentEqual(left, right []llm.ContentBlock) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	var leftValue, rightValue any
	if json.Unmarshal(leftJSON, &leftValue) != nil || json.Unmarshal(rightJSON, &rightValue) != nil {
		return false
	}
	return reflect.DeepEqual(leftValue, rightValue)
}

// goalRef is the exact current ref for a view.
func goalRef(view *goal.GoalView) goal.GoalRef {
	return goal.GoalRef{ID: view.ID, Revision: view.Revision}
}

// renderThrown renders human-readable unexpected values for logs.
func renderThrown(err error) string {
	return err.Error()
}

func (s *Service) warnf(format string, args ...any) {
	s.config.Logger.Warn(fmt.Sprintf(format, args...))
}

// --- state table -------------------------------------------------------------

func (s *Service) stateFor(agentRef *agent.Agent) *driverState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stateForLocked(agentRef)
}

// stateForLocked creates state for an exact currently live agent.
func (s *Service) stateForLocked(agentRef *agent.Agent) *driverState {
	if existing, ok := s.states[agentRef]; ok {
		return existing
	}
	state := &driverState{agent: agentRef}
	s.states[agentRef] = state
	return state
}

// currentGoalLocked reads only when the exact Agent remains live; errors are
// swallowed to an absent goal.
func (s *Service) currentGoalLocked(state *driverState) *goal.GoalView {
	view, _ := s.currentGoalCheckedLocked(state)
	return view
}

// currentGoalCheckedLocked surfaces fold failures so the pre-step check can
// fail closed with a disarm, exactly like the official try/catch.
func (s *Service) currentGoalCheckedLocked(state *driverState) (*goal.GoalView, error) {
	if s.agents.Get(state.agent.ID) != state.agent {
		return nil, nil
	}
	view, err := s.goals.Get(state.agent)
	if view == nil {
		return nil, err
	}
	return view, nil
}

// readyToDriveLocked reports whether this exact lifecycle is quiescent with
// no competing prompt.
func (s *Service) readyToDriveLocked(state *driverState) bool {
	return !s.closed &&
		!state.stopping &&
		s.agents.Get(state.agent.ID) == state.agent &&
		state.agent.Status() == agent.AgentIdle &&
		!state.competingQueued
}

// readyAfterCheckpointLocked rechecks every condition that an awaited
// checkpoint may have changed.
func (s *Service) readyAfterCheckpointLocked(state *driverState) bool {
	return s.readyToDriveLocked(state) && !state.needsCheckpoint
}

func (s *Service) flushSession(sess *session.Session) error {
	if s.config.Flusher != nil {
		return s.config.Flusher.FlushSession(sess)
	}
	if s.sessions == nil {
		return nil
	}
	return s.sessions.Flush().Error
}

// disarm removes automatic authority while preserving the durable phase.
func (s *Service) disarm(state *driverState) {
	s.mu.Lock()
	view := s.currentGoalLocked(state)
	agentRef := state.agent
	s.mu.Unlock()
	if view == nil || view.Activation != goal.ActivationArmed {
		return
	}
	if _, err := s.goals.Disarm(agentRef); err != nil {
		s.warnf("goal-round-driver: could not disarm agent %q: %s", agentRef.ID, renderThrown(err))
	}
}

// restoreOtherClaimed preserves claimed step context when this driver drops
// only its own round.
func (s *Service) restoreOtherClaimed(agentRef *agent.Agent, messages []llm.Message, messageID llm.MessageID) {
	for index := len(messages) - 1; index >= 0; index-- {
		message := messages[index]
		if message.ID == messageID {
			continue
		}
		if message.Source.Kind == llm.SourceGoal && message.Source.GoalRound == 0 {
			continue
		}
		if containsMessage(agentRef.Inbox.NextStep(), message.ID) ||
			containsMessage(agentRef.Inbox.NextTurn(), message.ID) {
			continue
		}
		if err := agentRef.Inbox.Prepend(agent.InboxNextStep, message); err != nil {
			s.warnf("goal-round-driver: could not restore claimed message %q for agent %q: %s",
				message.ID, agentRef.ID, renderThrown(err))
		}
	}
}

func containsMessage(messages []llm.Message, id llm.MessageID) bool {
	for _, candidate := range messages {
		if candidate.ID == id {
			return true
		}
	}
	return false
}

// followup queues the round through the loop driver; a missing driver is a
// queue failure, not a crash.
func (s *Service) followup(agentRef *agent.Agent, message llm.Message) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%v", recovered)
		}
	}()
	driver := agentRef.Driver()
	if driver == nil {
		return fmt.Errorf("agent %q has no driver installed", agentRef.ID)
	}
	driver.Followup(message)
	return nil
}

// --- the serialized driver -----------------------------------------------------

// requestDrive coalesces triggers onto one agent-local serialized driver.
func (s *Service) requestDrive(state *driverState) {
	s.mu.Lock()
	if state.stopping || s.closed {
		s.mu.Unlock()
		return
	}
	state.requested = true
	if state.running {
		s.mu.Unlock()
		return
	}
	state.running = true
	state.done = make(chan struct{})
	s.mu.Unlock()
	go s.driveLoop(state)
}

func (s *Service) driveLoop(state *driverState) {
	defer func() {
		s.mu.Lock()
		state.running = false
		done := state.done
		state.done = nil
		retrigger := state.requested && !state.stopping && !s.closed
		s.mu.Unlock()
		close(done)
		if retrigger {
			s.requestDrive(state)
		}
	}()
	err := s.agents.WithoutInitiator(context.Background(), func(ctx context.Context) error {
		for {
			s.mu.Lock()
			if !state.requested || state.stopping {
				s.mu.Unlock()
				return nil
			}
			state.requested = false
			s.mu.Unlock()
			if err := s.drive(state); err != nil {
				s.warnf("goal-round-driver: driver failed for agent %q: %s", state.agent.ID, renderThrown(err))
				s.disarm(state)
			}
		}
	})
	if err != nil {
		s.warnf("goal-round-driver: could not start driver for agent %q: %s", state.agent.ID, renderThrown(err))
		s.disarm(state)
	}
}

// drive processes admitted work at quiescence, then reserves at most one
// next round.
func (s *Service) drive(state *driverState) error {
	s.mu.Lock()
	if !s.readyToDriveLocked(state) {
		s.mu.Unlock()
		return nil
	}
	if state.needsCheckpoint {
		state.needsCheckpoint = false
		agentRef := state.agent
		s.mu.Unlock()
		flushErr := s.flushSession(agentRef.Session)
		s.mu.Lock()
		if flushErr != nil {
			s.mu.Unlock()
			s.warnf("goal-round-driver: durability checkpoint failed for agent %q: %s",
				agentRef.ID, renderThrown(flushErr))
			s.disarm(state)
			return nil
		}
		// A mutation or ordinary prompt may have arrived while the
		// checkpoint was settling. Give it its own checkpoint / turn
		// before reserving.
		if !s.readyAfterCheckpointLocked(state) {
			s.mu.Unlock()
			return nil
		}
	}
	if state.attempt != nil {
		state.attempt = nil
		state.needsCheckpoint = true
		state.requested = true
		s.mu.Unlock()
		return nil
	}
	view := s.currentGoalLocked(state)
	if view == nil || view.Phase != goal.PhaseActive || view.Activation != goal.ActivationArmed {
		s.mu.Unlock()
		return nil
	}
	if view.RoundsStarted >= view.MaxGoalRounds {
		agentRef := state.agent
		ref := goalRef(view)
		limit := view.MaxGoalRounds
		s.mu.Unlock()
		_, err := s.goals.Block(agentRef, ref, goal.GoalBlockReason{
			Code:    "round-limit",
			Message: fmt.Sprintf("Goal reached its configured limit of %d rounds.", limit),
		})
		return err
	}
	round := view.RoundsStarted + 1
	content := RenderGoalRoundPrompt(view, round)
	message := llm.NewUserMessage(content, llm.MessageSource{
		Kind: llm.SourceGoal, GoalID: string(view.ID), GoalRevision: view.Revision, GoalRound: round,
	})
	state.attempt = &roundAttempt{
		goalID: view.ID, revision: view.Revision, round: round,
		messageID: message.ID, content: content, phase: attemptQueued,
	}
	agentRef := state.agent
	goalID, goalRevision := view.ID, view.Revision
	s.mu.Unlock()
	if err := s.followup(agentRef, message); err != nil {
		s.mu.Lock()
		state.attempt = nil
		s.mu.Unlock()
		s.warnf("goal-round-driver: could not queue round %d for agent %q: %s",
			round, agentRef.ID, renderThrown(err))
		s.mu.Lock()
		latest := s.currentGoalLocked(state)
		s.mu.Unlock()
		if latest != nil && latest.ID == goalID && latest.Revision == goalRevision &&
			latest.Phase == goal.PhaseActive && latest.Activation == goal.ActivationArmed {
			if _, blockErr := s.goals.Block(agentRef, goalRef(latest), goal.GoalBlockReason{
				Code:    "queue-failed",
				Message: fmt.Sprintf("Could not queue goal round %d: %s", round, renderThrown(err)),
			}); blockErr != nil {
				return blockErr
			}
		}
	}
	return nil
}

// --- event wiring --------------------------------------------------------------

func (s *Service) register(agents *agent.AgentRegistry) []func() {
	bus := agents.Events()
	disposers := []func(){
		bus.OnEmit(agent.EventAgentError, nil, func(payload any) error {
			s.disarm(s.stateFor(payload.(agent.AgentErrorPayload).Agent))
			return nil
		}),
		bus.OnEmit(agent.EventAgentCreated, nil, func(payload any) error {
			s.stateFor(payload.(agent.AgentLifecyclePayload).Agent)
			return nil
		}),
		bus.OnEmit(agent.EventAgentDisposed, nil, func(payload any) error {
			agentRef := payload.(agent.AgentLifecyclePayload).Agent
			s.mu.Lock()
			delete(s.states, agentRef)
			s.mu.Unlock()
			return nil
		}),
		bus.OnEmit(agent.EventAgentSessionStart, nil, func(payload any) error {
			state := s.stateFor(payload.(agent.AgentSessionStartPayload).Agent)
			s.mu.Lock()
			state.attempt = nil
			state.competingQueued = false
			state.needsCheckpoint = false
			s.mu.Unlock()
			return nil
		}),
		bus.OnEmit(agent.EventAgentStatus, nil, func(payload any) error {
			s.onStatus(payload.(agent.AgentStatusPayload))
			return nil
		}),
		bus.OnEmit(goal.EventChanged, nil, func(payload any) error {
			state := s.stateFor(payload.(goal.ChangedPayload).Agent)
			s.mu.Lock()
			state.needsCheckpoint = true
			s.mu.Unlock()
			s.requestDrive(state)
			return nil
		}),
		bus.OnEmit(agent.EventInboxInserted, nil, func(payload any) error {
			s.onInboxInserted(payload.(agent.AgentMessagePayload))
			return nil
		}),
		bus.InboxClaimed().On(nil, func(payload agent.AgentClaimedPayload) error {
			s.mu.Lock()
			state := s.stateForLocked(payload.Agent)
			if state.attempt != nil && sameQueued(payload.Message.Content, payload.Message.Source, state.attempt) {
				state.attempt.phase = attemptClaimed
			}
			s.mu.Unlock()
			return nil
		}),
		bus.InboxDiscarded().On(nil, func(payload agent.AgentMessagePayload) error {
			s.mu.Lock()
			state := s.stateForLocked(payload.Agent)
			if state.attempt != nil && sameQueued(payload.Message.Content, payload.Message.Source, state.attempt) {
				state.attempt.cancelled = true
			}
			s.mu.Unlock()
			return nil
		}),
		s.registerPreStep(bus),
	}
	return disposers
}

func (s *Service) onStatus(payload agent.AgentStatusPayload) {
	s.mu.Lock()
	state := s.stateForLocked(payload.Agent)
	if payload.Status != agent.AgentIdle {
		s.mu.Unlock()
		return
	}
	state.competingQueued = false
	attempt := state.attempt
	view := s.currentGoalLocked(state)
	pauseNeeded := attempt != nil &&
		(attempt.phase == attemptQueued || attempt.phase == attemptClaimed || attempt.cancelled) &&
		view != nil && view.Phase == goal.PhaseActive && view.Activation == goal.ActivationArmed
	var ref goal.GoalRef
	if pauseNeeded {
		state.attempt = nil
		ref = goalRef(view)
	}
	agentRef := payload.Agent
	s.mu.Unlock()
	if pauseNeeded {
		if _, err := s.goals.Pause(agentRef, ref); err != nil {
			s.warnf("goal-round-driver: could not pause cancelled goal for agent %q: %s",
				agentRef.ID, renderThrown(err))
			s.disarm(state)
		}
	}
	s.requestDrive(state)
}

func (s *Service) onInboxInserted(payload agent.AgentMessagePayload) {
	if !containsMessage(payload.Agent.Inbox.NextTurn(), payload.Message.ID) {
		return
	}
	s.mu.Lock()
	state := s.stateForLocked(payload.Agent)
	if state.attempt != nil && sameQueued(payload.Message.Content, payload.Message.Source, state.attempt) {
		s.mu.Unlock()
		return
	}
	state.competingQueued = true
	if state.attempt != nil && state.attempt.phase == attemptQueued {
		state.attempt.stale = true
	}
	s.mu.Unlock()
}

// OnSessionEvent observes one durable event for the session's exact live
// agent: admitted rounds advance the attempt phase, and terminal turns apply
// the official disarm / cancel fence. Exported for the owning session seam.
func (s *Service) OnSessionEvent(sess *session.Session, event session.Event) {
	agentRef := s.agents.Get(sess.ID())
	if agentRef == nil || agentRef.Session != sess {
		return
	}
	switch event.Type {
	case session.EventUserMessage:
		var data struct {
			ID llm.MessageID `json:"id"`
		}
		if json.Unmarshal(event.Data, &data) != nil {
			return
		}
		s.mu.Lock()
		state := s.stateForLocked(agentRef)
		if state.attempt != nil && data.ID == state.attempt.messageID {
			state.attempt.phase = attemptAdmitted
		}
		s.mu.Unlock()
	case session.EventTurnEnd:
		var data session.TurnEndData
		if json.Unmarshal(event.Data, &data) != nil {
			return
		}
		switch data.Reason.Kind {
		case session.TurnEndMaxTokens:
			s.disarm(s.stateFor(agentRef))
		case session.TurnEndAborted:
			s.mu.Lock()
			state := s.stateForLocked(agentRef)
			cancelAttempt := state.attempt != nil &&
				(state.attempt.phase == attemptClaimed || state.attempt.phase == attemptAdmitted)
			if cancelAttempt {
				state.attempt.cancelled = true
			}
			s.mu.Unlock()
			if !cancelAttempt {
				s.disarm(state)
			}
		}
	}
}

// --- the pre-step fence ----------------------------------------------------------

// validReservation fails closed unless the queued prompt still owns the
// exact live revision. A fold failure surfaces as an error so the caller
// disarms, exactly like the official try/catch.
func (s *Service) validReservation(state *driverState, content []llm.ContentBlock,
	source llm.MessageSource,
) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || state.stopping || state.attempt == nil ||
		state.attempt.phase != attemptClaimed || state.attempt.stale {
		return false, nil
	}
	if !sameQueued(content, source, state.attempt) {
		return false, nil
	}
	view, err := s.currentGoalCheckedLocked(state)
	if err != nil {
		return false, err
	}
	if view == nil || view.ID != goal.GoalID(source.GoalID) || view.Revision != source.GoalRevision ||
		view.Phase != goal.PhaseActive || view.Activation != goal.ActivationArmed ||
		source.GoalRound != view.RoundsStarted+1 {
		return false, nil
	}
	return true, nil
}

func (s *Service) registerPreStep(bus *agent.SubjectEventBus) func() {
	return bus.PreStep().On(nil, func(payload agent.PreStepPayload,
		next func(agent.PreStepPayload) agent.PreStepDecision,
	) agent.PreStepDecision {
		var submitted *llm.Message
		for index := range payload.Messages {
			if isGoalRoundSource(payload.Messages[index].Source) {
				submitted = &payload.Messages[index]
				break
			}
		}
		if submitted == nil {
			return next(payload)
		}
		content, source := submitted.Content, submitted.Source
		state := s.stateFor(payload.Agent)
		valid, checkErr := s.validReservation(state, content, source)
		if checkErr != nil {
			s.warnf("goal-round-driver: pre-step check failed for agent %q: %s",
				payload.Agent.ID, renderThrown(checkErr))
			s.disarm(state)
			valid = false
		}
		if !valid {
			s.mu.Lock()
			if state.attempt != nil && sameRound(source, state.attempt) {
				state.attempt.stale = true
				state.attempt = nil
			}
			s.mu.Unlock()
			s.restoreOtherClaimed(payload.Agent, payload.Messages, submitted.ID)
			s.requestDrive(state)
			return agent.PreStepReject()
		}
		var decision agent.PreStepDecision
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					if payload.Signal != nil && payload.Signal.Err() != nil {
						panic(recovered)
					}
					// A throwing downstream hook drops the whole step
					// proposal. Clear the reservation before the balanced
					// no-step turn returns to idle so the next drive pass
					// can reschedule the round.
					s.mu.Lock()
					state.attempt = nil
					s.mu.Unlock()
					s.requestDrive(state)
					panic(recovered)
				}
			}()
			decision = next(payload)
		}()
		if payload.Signal != nil && payload.Signal.Err() != nil {
			if decision.Kind == "enter" {
				s.restoreOtherClaimed(payload.Agent, decision.Messages, submitted.ID)
			}
			return decision
		}
		if decision.Kind == "reject" {
			s.mu.Lock()
			state.attempt = nil
			view := s.currentGoalLocked(state)
			blockNeeded := view != nil && view.ID == goal.GoalID(source.GoalID) &&
				view.Revision == source.GoalRevision &&
				view.Phase == goal.PhaseActive && view.Activation == goal.ActivationArmed
			var ref goal.GoalRef
			if blockNeeded {
				ref = goalRef(view)
			}
			s.mu.Unlock()
			if blockNeeded {
				// The official throw escalates into the turn; Go cannot
				// throw through the waterfall boundary, so a failed block
				// is warned instead (recorded deviation).
				if _, err := s.goals.Block(payload.Agent, ref, goal.GoalBlockReason{
					Code:    "prompt-rejected",
					Message: "Goal round was rejected before entering its step.",
				}); err != nil {
					s.warnf("goal-round-driver: could not block rejected round for agent %q: %s",
						payload.Agent.ID, renderThrown(err))
				}
			}
			return decision
		}
		valid, checkErr = s.validReservation(state, content, source)
		if checkErr != nil {
			s.warnf("goal-round-driver: post-decision check failed for agent %q: %s",
				payload.Agent.ID, renderThrown(checkErr))
			s.disarm(state)
			valid = false
		}
		if !valid {
			s.mu.Lock()
			state.attempt = nil
			s.mu.Unlock()
			s.restoreOtherClaimed(payload.Agent, decision.Messages, submitted.ID)
			s.requestDrive(state)
			return agent.PreStepReject()
		}
		decision.StartsRequestSeries = true
		return decision
	})
}

// --- teardown -------------------------------------------------------------------

func (s *Service) stop() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	states := make([]*driverState, 0, len(s.states))
	for _, state := range s.states {
		state.stopping = true
		states = append(states, state)
	}
	s.mu.Unlock()

	var waits []<-chan struct{}
	for _, state := range states {
		s.disarm(state)
		s.mu.Lock()
		attempt := state.attempt
		if attempt != nil {
			attempt.stale = true
		}
		agentRef := state.agent
		s.mu.Unlock()
		if attempt != nil && agentRef.Status() == agent.AgentRunning {
			agentRef.Cancel(session.TurnEndCancelCause{Kind: session.CancelParent}, agent.CancelOptions{})
			if driver := agentRef.Driver(); driver != nil {
				waits = append(waits, driver.WhenIdle())
			}
		}
		s.mu.Lock()
		if state.done != nil {
			waits = append(waits, state.done)
		}
		s.mu.Unlock()
	}
	for _, wait := range waits {
		<-wait
	}
	s.mu.Lock()
	s.states = map[*agent.Agent]*driverState{}
	s.mu.Unlock()
}

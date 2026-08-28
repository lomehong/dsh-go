package agentloop

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sync"

	"dshgo/agent"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/systemprompt"
)

// Default Agent driver over queued turns and step-boundary input. Every
// request is derived from the session log.
//
// Port of packages/core/agent-loop/src/agent.ts. Go adaptations: the JS
// `Promise` activity handle is a closed-on-settle channel with a generation
// check; AbortController is a context.Context with WithCancelCause (the cause
// value is the TurnEndCancelCause); phase transitions happen under one mutex
// while the driver goroutine performs all session appends, preserving the
// source's single-threaded event-loop ordering structurally.

// Driver phase kinds.
const (
	phaseIdle        = "idle"
	phaseMaintenance = "maintenance"
	phaseRunning     = "running"
)

// driverPhase is the driver's committed activity state.
type driverPhase struct {
	kind          string
	goCtx         context.Context
	cancel        context.CancelCauseFunc
	lastTurn      int64
	turn          int64
	step          int64
	wakeRequested bool
}

// ReactLoopAgent drives one session through turn and step boundaries.
type ReactLoopAgent struct {
	*agent.Agent
	loop *AgentLoop

	mu          sync.Mutex
	phase       driverPhase
	activity    chan struct{}
	activityGen uint64

	// Whether this loop instance has appended its initial/resume request anchor.
	requestHeaderLogged bool
	// Surface generation of the preceding built request.
	hasRequestSurfaceGeneration bool
	requestSurfaceGeneration    int64

	runtimeContext *RuntimeContextProjection
}

// NewReactLoopAgent builds the driver over one registered agent. The caller
// owns registration (Enter/Announce) and teardown; the driver installs itself
// as the agent's Driver.
func NewReactLoopAgent(loop *AgentLoop, a *agent.Agent) *ReactLoopAgent {
	driver := &ReactLoopAgent{Agent: a, loop: loop}
	driver.phase = driverPhase{kind: phaseIdle}
	var lastTurn int64
	for index := len(a.Session.Events()) - 1; index >= 0; index -= 1 {
		event := a.Session.Events()[index]
		if event.Type != session.EventTurnStart {
			continue
		}
		var data session.TurnStartData
		if err := json.Unmarshal(event.Data, &data); err == nil {
			lastTurn = data.Turn
		}
		break
	}
	driver.phase.lastTurn = lastTurn
	driver.activity = closedChannel()
	driver.runtimeContext = NewRuntimeContextProjection(a.Session)
	a.SetDriver(driver)
	return driver
}

func closedChannel() chan struct{} {
	closed := make(chan struct{})
	close(closed)
	return closed
}

// status derives the externally visible status from the phase.
func (d *ReactLoopAgent) status() agent.AgentStatus {
	if d.phase.kind == phaseRunning {
		return agent.AgentRunning
	}
	return agent.AgentIdle
}

// setPhase commits a phase and publishes its externally visible status
// transition.
func (d *ReactLoopAgent) setPhase(next driverPhase) {
	d.mu.Lock()
	previous := d.status()
	d.phase = next
	status := d.status()
	d.mu.Unlock()
	if status != previous {
		d.SetStatus(status)
	}
}

// Send routes identified input to an inbox boundary and optionally wakes the
// driver.
func (d *ReactLoopAgent) Send(message llm.Message, target agent.InboxTarget, wakeup bool) {
	// Waking input cannot join an aborted activity, so it starts the next
	// turn. Captured before the insertion so a reentrant cancel from a splice
	// observer cannot reclassify it.
	d.mu.Lock()
	wakingAfterAbort := wakeup && d.phase.kind != phaseIdle && d.phase.goCtx != nil && d.phase.goCtx.Err() != nil
	resolvedTarget := target
	if wakingAfterAbort {
		resolvedTarget = agent.InboxNextTurn
	}
	d.mu.Unlock()
	if _, err := d.Inbox.Splice(resolvedTarget, math.MaxInt64, 0, []llm.Message{message}); err != nil {
		d.emitLoopError(err)
		return
	}
	if wakeup {
		d.wakeDriver(wakingAfterAbort)
	}
}

// Followup queues an ordinary follow-up turn and wakes the driver.
func (d *ReactLoopAgent) Followup(message llm.Message) {
	d.Send(message, agent.InboxNextTurn, true)
}

// Steer submits steering for the nearest step boundary.
func (d *ReactLoopAgent) Steer(message llm.Message) {
	d.Send(message, agent.InboxNextStep, true)
}

// Inject queues model-facing context for the next pre-step without waking the
// driver.
func (d *ReactLoopAgent) Inject(message llm.Message) {
	d.Send(message, agent.InboxNextStep, false)
}

// Cancel clears queued and steering work — unless options.KeepInbox — and
// aborts the active turn or between-turn task.
func (d *ReactLoopAgent) Cancel(cause session.TurnEndCancelCause, options agent.CancelOptions) {
	if !options.KeepInbox {
		if err := d.Inbox.Clear(); err != nil {
			d.emitLoopError(err)
			return
		}
		d.mu.Lock()
		if d.phase.kind != phaseIdle {
			d.phase.wakeRequested = false
		}
		d.mu.Unlock()
	}
	d.mu.Lock()
	phase := d.phase
	cancel := phase.cancel
	d.mu.Unlock()
	if phase.kind != phaseIdle && cancel != nil {
		cancel(wrapCause(cause))
	}
}

// WhenIdle resolves after the current whole-agent activity reaches
// quiescence, following replacement work started before the observed driver
// retires.
func (d *ReactLoopAgent) WhenIdle() <-chan struct{} {
	done := make(chan struct{})
	go func() {
		for {
			d.mu.Lock()
			current := d.activity
			d.mu.Unlock()
			<-current
			d.mu.Lock()
			replaced := d.activity != current
			d.mu.Unlock()
			if !replaced {
				close(done)
				return
			}
		}
	}()
	return done
}

// RunMaintenance runs one non-turn maintenance task from the true idle phase;
// it fails when turn-driving or another maintenance task already owns the
// agent. The task runs to quiescence before the phase returns to idle, so the
// synchronous return is the source's awaited promise.
func (d *ReactLoopAgent) RunMaintenance(task func(signal context.Context) error) error {
	d.mu.Lock()
	if d.phase.kind != phaseIdle {
		d.mu.Unlock()
		return fmt.Errorf("agent %q already has active work", d.ID)
	}
	goCtx, cancel := context.WithCancelCause(d.loop.baseCtx)
	maintenance := driverPhase{kind: phaseMaintenance, goCtx: goCtx, cancel: cancel, lastTurn: d.phase.lastTurn}
	done := make(chan struct{})
	d.activity = done
	d.setPhaseLocked(maintenance)
	d.mu.Unlock()

	err := task(goCtx)
	cancel(nil)
	d.mu.Lock()
	wakeRequested := d.phase.wakeRequested
	d.setPhaseLocked(driverPhase{kind: phaseIdle, lastTurn: maintenance.lastTurn})
	wake := wakeRequested && d.Inbox.HasPending()
	d.mu.Unlock()
	close(done)
	if wake {
		d.wakeDriver(false)
	}
	return err
}

// setPhaseLocked installs a phase while the caller holds d.mu, publishing any
// status transition.
func (d *ReactLoopAgent) setPhaseLocked(next driverPhase) {
	previous := d.status()
	d.phase = next
	status := d.status()
	if status != previous {
		d.SetStatus(status)
	}
}

// wakeDriver starts one driver, or latches its wake behind maintenance or an
// aborted activity. A wake sent while idle always opens its turn boundary,
// even when its message was cleared; only a latched replay is suppressed when
// the queue no longer holds the wake.
func (d *ReactLoopAgent) wakeDriver(wakeAfterAbort bool) {
	d.mu.Lock()
	if d.phase.kind != phaseIdle {
		// Maintenance and aborted drivers cannot deliver the wake: latch it
		// for replay at convergence. Live drivers claim queued work
		// themselves; disposal never latches, so teardown waits on no model
		// turn.
		latch := d.phase.kind == phaseMaintenance || wakeAfterAbort
		if latch {
			disposed := false
			if d.phase.goCtx != nil {
				if _, isDisposed := unwrapCause(context.Cause(d.phase.goCtx)); isDisposed {
					disposed = true
				}
			}
			if !disposed {
				d.phase.wakeRequested = true
			}
		}
		d.mu.Unlock()
		return
	}
	goCtx, cancel := context.WithCancelCause(d.loop.baseCtx)
	done := make(chan struct{})
	d.activityGen++
	d.activity = done
	activityGen := d.activityGen
	d.phase = driverPhase{
		kind:   phaseRunning,
		goCtx:  goCtx,
		cancel: cancel,
		turn:   d.phase.lastTurn,
	}
	d.setPhaseLocked(d.phase)
	d.mu.Unlock()

	go func() {
		defer close(done)
		// The whole driver chain runs inside one initiator boundary; the
		// follow-up turn reuses the same boundary context.
		if err := d.loop.Registry.WithInitiator(goCtx, d.Agent, func(ctx context.Context) error {
			d.kick(ctx)
			return nil
		}); err != nil {
			d.emitLoopError(err)
		}
		d.mu.Lock()
		// A driver that was replaced must not retire its successor's phase.
		if d.activityGen != activityGen || d.phase.kind != phaseRunning {
			d.mu.Unlock()
			return
		}
		wakeRequested := d.phase.wakeRequested
		d.phase = driverPhase{kind: phaseIdle, lastTurn: d.phase.turn}
		wake := wakeRequested && d.Inbox.HasPending()
		d.setPhaseLocked(d.phase)
		d.mu.Unlock()
		if wake {
			d.wakeDriver(false)
		}
	}()
}

// emitLoopError reports one driver-external failure at its live boundary.
func (d *ReactLoopAgent) emitLoopError(err error) {
	d.Events().Emit(agent.EventAgentError, d.Scope, agent.AgentErrorPayload{Agent: d.Agent, Error: err})
}

// throwError reports one failure at its live boundary, then preserves it for
// driver containment.
func (d *ReactLoopAgent) throwError(turn, step int64, err error) error {
	d.Events().Emit(agent.EventAgentError, d.Scope, agent.AgentErrorPayload{Agent: d.Agent, Turn: turn, Step: step, Error: err})
	return err
}

// kick drives turns until the session quiesces. Reported failures and
// cancellation are contained at the driver boundary.
func (d *ReactLoopAgent) kick(signal context.Context) {
	for {
		cont, err := d.turn(signal)
		if err != nil || !cont {
			break
		}
	}
}

// turn opens one turn before claiming its first proposed step. It returns
// whether a follow-up turn continues the driver.
func (d *ReactLoopAgent) turn(goCtx context.Context) (cont bool, retErr error) {
	d.mu.Lock()
	phase := d.phase
	d.mu.Unlock()
	if phase.kind != phaseRunning {
		return false, d.throwError(0, 0, fmt.Errorf("agent %q: turn without driver reservation", d.ID))
	}
	signal := goCtx
	if err := signal.Err(); err != nil {
		return false, err
	}
	turn := phase.turn + 1
	if _, err := d.Session.Append(session.EventTurnStart, session.TurnStartData{Turn: turn}, nil); err != nil {
		return false, d.throwError(turn, 0, err)
	}
	d.mu.Lock()
	d.phase.turn = turn
	d.mu.Unlock()

	var turnEnds *session.TurnEndReason
	var turnStep int64
	defer func() {
		// Every exit assigns a turn ending; a nil ending encodes as the zero
		// value (kind-less), matching the source's null reason.
		var reasonValue session.TurnEndReason
		if turnEnds != nil {
			reasonValue = *turnEnds
		}
		if _, err := d.Session.Append(session.EventTurnEnd, session.TurnEndData{Turn: turn, Reason: reasonValue}, nil); err != nil {
			retErr = d.throwError(turn, turnStep, err)
			cont = false
		}
	}()

	// fail structures every abnormal exit: an aborted signal becomes the
	// aborted ending; any other failure becomes a structured error ending,
	// reported at its live boundary first.
	fail := func(err error, atStep int64) (bool, error) {
		if signal.Err() != nil {
			var cause session.TurnEndCancelCause
			if adapted, isCancel := unwrapCause(context.Cause(signal)); isCancel {
				cause = adapted
			}
			turnEnds = &session.TurnEndReason{Kind: session.TurnEndAborted, Reason: &cause}
			return false, err
		}
		// Every failure is structured: an LlmError keeps its facts, anything
		// else flattens to errorChain text under the UNKNOWN code.
		failure := llm.NormalizeLlmFailure(err)
		turnEnds = &session.TurnEndReason{Kind: session.TurnEndError, Error: &failure}
		return false, d.throwError(turn, atStep, err)
	}

	target := agent.InboxNextTurn
	for {
		if err := signal.Err(); err != nil {
			return fail(err, turnStep)
		}
		decision, err := d.preStep(signal, target, turn, turnStep+1)
		if err != nil {
			return fail(err, turnStep)
		}
		if decision.reject {
			turnEnds = &session.TurnEndReason{Kind: session.TurnEndBlocked}
			return false, nil
		}
		if turnEnds != nil && len(decision.messages) == 0 {
			break
		}
		// A removed waking message or an enter decision rewritten to empty
		// still owns the initial turn boundary, but it spends no model call.
		if turnStep == 0 && len(decision.messages) == 0 {
			turnEnds = &session.TurnEndReason{Kind: session.TurnEndCompleted}
			return false, nil
		}
		if err := signal.Err(); err != nil {
			return fail(err, turnStep)
		}
		step := turnStep + 1
		if _, err := d.Session.Append(session.EventStepStart, session.StepStartData{Turn: turn, Step: step}, nil); err != nil {
			return fail(err, step)
		}
		d.mu.Lock()
		d.phase.step = step
		d.mu.Unlock()

		// The claimed decision messages persist inside the step, so the
		// step/end boundary closes even when an append fails.
		stepErr := func() (stepErr error) {
			for _, message := range decision.messages {
				if _, err := d.Session.Append(session.EventUserMessage, message, &session.SurfaceIntent{
					SurfaceOp: session.SurfaceOp{Kind: session.SurfaceAppend},
				}); err != nil {
					return err
				}
			}
			return d.runStep(signal, turn, step, decision, &turnEnds)
		}()
		// The step boundary closes even on failure; a boundary append error
		// surfaces only when the step itself did not already fail.
		if appendErr := d.appendStepEnd(turn, step); appendErr != nil && stepErr == nil {
			stepErr = appendErr
		}
		if stepErr != nil {
			return fail(stepErr, step)
		}

		if err := signal.Err(); err != nil {
			return fail(err, step)
		}
		if turnEnds != nil && len(d.Inbox.NextStep()) == 0 {
			d.Events().Serial(agent.EventTurnStopping, d.Scope, agent.TurnStoppingPayload{Agent: d.Agent, Turn: turn, Signal: signal})
			if err := signal.Err(); err != nil {
				return fail(err, step)
			}
		}
		if turnEnds != nil && len(d.Inbox.NextStep()) == 0 {
			break
		}
		turnStep = step
		target = agent.InboxNextStep
	}

	if !d.Inbox.HasPending() {
		return false, nil
	}
	// The stale-latch guard: the live driver claims the queue itself, so a
	// wake latched against the finished turn never replays twice.
	d.mu.Lock()
	d.phase.wakeRequested = false
	d.phase.step = 0
	d.mu.Unlock()
	return true, nil
}

// cancelCauseError adapts the structured cancel cause into the error value
// context.CancelCauseFunc carries.
type cancelCauseError struct{ cause session.TurnEndCancelCause }

func (e cancelCauseError) Error() string {
	if e.cause.Reason != "" {
		return "agent cancelled: " + e.cause.Kind + ": " + e.cause.Reason
	}
	return "agent cancelled: " + e.cause.Kind
}

func wrapCause(cause session.TurnEndCancelCause) error { return cancelCauseError{cause: cause} }

func unwrapCause(err error) (session.TurnEndCancelCause, bool) {
	if adapted, ok := err.(cancelCauseError); ok {
		return adapted.cause, true
	}
	return session.TurnEndCancelCause{}, false
}
func (d *ReactLoopAgent) appendStepEnd(turn, step int64) error {
	_, err := d.Session.Append(session.EventStepEnd, session.StepEndData{Turn: turn, Step: step}, nil)
	return err
}

// runStep executes one step body, closing its boundary even on failure.
func (d *ReactLoopAgent) runStep(signal context.Context, turn, step int64, decision preparedStep, turnEnds **session.TurnEndReason) (stepErr error) {
	defer func() {
		if rec := recover(); rec != nil && stepErr == nil {
			stepErr = fmt.Errorf("step %d.%d failed: %v", turn, step, rec)
		}
	}()
	stepEnd, err := d.step(signal, turn, step, decision.assembly, decision.startsRequestSeries)
	if err != nil {
		return err
	}
	// max-tokens is sticky: once any step hits the ceiling, later steps that
	// complete normally must not downgrade the turn outcome.
	if *turnEnds == nil || (*turnEnds).Kind != session.TurnEndMaxTokens {
		*turnEnds = &stepEnd
	}
	return nil
}

// preStep claims the proposed step's input and resolves the pre-step
// waterfall over it.
func (d *ReactLoopAgent) preStep(signal context.Context, target agent.InboxTarget, turn, step int64) (preparedStep, error) {
	d.mu.Lock()
	if d.phase.kind != phaseRunning {
		d.mu.Unlock()
		return preparedStep{}, fmt.Errorf("agent %q: pre-step outside running phase", d.ID)
	}
	d.mu.Unlock()
	claimed, err := d.Inbox.Claim(target, turn)
	if err != nil {
		return preparedStep{}, err
	}
	assembly, err := d.loop.Prompt.Assemble(d.AssembleContextFor(signal))
	if err != nil {
		return preparedStep{}, err
	}
	if err := signal.Err(); err != nil {
		return preparedStep{}, err
	}
	sections, err := systemprompt.RenderContextSections(assembly)
	if err != nil {
		return preparedStep{}, err
	}
	joined := systemprompt.JoinContextSections(sections)
	projected := d.runtimeContext.Project(joined, sections)
	messages := claimed
	if projected.ID != "" || len(projected.Content) > 0 {
		messages = append(append([]llm.Message{}, claimed...), projected)
	}
	decision := d.Events().Waterfall(agent.EventPreStep, d.Scope, agent.PreStepPayload{
		Agent:    d.Agent,
		Messages: claimed,
		Turn:     turn,
		Step:     step,
		Signal:   signal,
	}, func(payload any) any {
		return agent.PreStepEnter(messages)
	}).(agent.PreStepDecision)
	if err := signal.Err(); err != nil {
		return preparedStep{}, err
	}
	if decision.Kind == "reject" {
		return preparedStep{reject: true}, nil
	}
	return preparedStep{
		messages:            decision.Messages,
		startsRequestSeries: decision.StartsRequestSeries,
		assembly:            assembly,
	}, nil
}

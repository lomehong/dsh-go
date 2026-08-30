package planmode

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/scope"
	"dshgo/session"
	"dshgo/session/projection"
	"dshgo/systemprompt"
)

// SectionName is the plan guidance prompt section's name; Order comes from
// the first-party section order.
const SectionName = "plan:policy"

// pluginName is the plugin-source attribution on narrations.
const pluginName = "plan-mode"

// Select outcomes of Set.
const (
	// OutcomeCommitted: logged now.
	OutcomeCommitted = "committed"
	// OutcomeQueued: awaiting the next accepted in-turn pre-step.
	OutcomeQueued = "queued"
	// OutcomeCancelled: an opposite pending selection was cleared; the
	// logged state already matches.
	OutcomeCancelled = "cancelled"
	// OutcomeNoop: already in that state.
	OutcomeNoop = "noop"
)

// pendingIntent is one selection awaiting the next accepted in-turn
// pre-step. Narrate is true for user selections and false for the exit
// tool, whose result already narrates the transition.
type pendingIntent struct {
	active  bool
	narrate bool
}

// Controller owns logged plan state, applies and narrates selected state at
// step start, and serves the `plan:policy` section. UIs observe committed
// flips through the session log; there is no live mirror. Go adaptation:
// the source's pending-intent WeakMap is a map keyed by session; entries
// are removed when consumed, and sessions are process-lifetime residents.
type Controller struct {
	mu      sync.Mutex
	section string
	pending map[*session.Session]pendingIntent
}

// NewController validates the deployment-owned guidance and builds the
// controller.
func NewController(section string) (*Controller, error) {
	resolved, err := ResolveSection(section)
	if err != nil {
		return nil, err
	}
	return &Controller{section: resolved, pending: map[*session.Session]pendingIntent{}}, nil
}

// Get reads the logged plan state and any selected state awaiting the next
// accepted in-turn pre-step.
func (c *Controller) Get(sess *session.Session) (active bool, pending bool, hasPending bool) {
	active = FoldPlanMode(sess.Events(), -1)
	c.mu.Lock()
	intent, ok := c.pending[sess]
	c.mu.Unlock()
	if !ok {
		return active, false, false
	}
	return active, intent.active, true
}

// Set selects whether plan mode should be active. Between turns the method
// appends the change immediately because no in-turn pre-step will run until
// another prompt starts a turn. The open-turn fold is the idle signal:
// agent status stays `running` through post-turn checkpointing, when no
// further in-turn pre-step runs. During an open turn the selection remains
// pending until the next accepted in-turn pre-step. Repeated selection of
// the current or already-pending state is a no-op. A failed durable append
// returns the error to the caller (the official append throws) with the
// pending selection — when one exists — left retryable.
func (c *Controller) Set(agentObj *agent.Agent, active bool) (string, error) {
	sess := agentObj.Session
	c.mu.Lock()
	pending, hasPending := c.pending[sess]
	c.mu.Unlock()
	target := active
	if hasPending {
		target = pending.active
	} else {
		target = FoldPlanMode(sess.Events(), -1)
	}
	if active == target {
		return OutcomeNoop, nil
	}
	if HasOpenTurn(sess.Events()) {
		c.mu.Lock()
		c.pending[sess] = pendingIntent{active: active, narrate: true}
		c.mu.Unlock()
		if FoldPlanMode(sess.Events(), -1) == active {
			return OutcomeCancelled, nil
		}
		return OutcomeQueued, nil
	}
	// No open turn: commit now. Delete only after append succeeds so a
	// failed durable write leaves the selection retryable, not dropped.
	if active == FoldPlanMode(sess.Events(), -1) {
		c.mu.Lock()
		delete(c.pending, sess)
		c.mu.Unlock()
		return OutcomeCancelled, nil
	}
	if _, err := sess.Append(EventPlanMode, PlanModeData{Active: active}, nil); err != nil {
		return "", err
	}
	c.mu.Lock()
	delete(c.pending, sess)
	c.mu.Unlock()
	if narration := c.narration(sess, active); narration != nil {
		if err := agentObj.Inbox.Append(agent.InboxNextTurn, *narration); err != nil {
			agentObj.Ctx.Logger().Warn(fmt.Sprintf("plan-mode: the plan change narration for agent %q was not delivered: %v", agentObj.ID, err))
		}
	}
	return OutcomeCommitted, nil
}

// QueueExit records the exit tool's approval: the selected inactive state
// waits for the next accepted in-turn pre-step. Narrate is false because the
// tool's own result already narrates the transition.
func (c *Controller) QueueExit(sess *session.Session) {
	c.mu.Lock()
	c.pending[sess] = pendingIntent{active: false, narrate: false}
	c.mu.Unlock()
}

// RegisterPreStep registers the pre-step waterfall listener for the
// lifetime of the composition. Pre-step is outside Session.append
// publication, so it can append the log-only mode event inside an open turn
// without re-entering the session. A failed append remains pending for a
// later accepted in-turn pre-step, and policy cannot block the step.
func (c *Controller) RegisterPreStep(agents *agent.AgentRegistry, logger cordis.Logger) func() {
	return agents.Events().PreStep().On(nil, func(preStep agent.PreStepPayload, next func(agent.PreStepPayload) agent.PreStepDecision) agent.PreStepDecision {
		decision := next(preStep)
		sess := preStep.Agent.Session
		c.mu.Lock()
		pending, hasPending := c.pending[sess]
		c.mu.Unlock()
		if decision.Kind == "reject" || preStep.Signal.Err() != nil || !hasPending {
			return decision
		}
		narration := c.narration(sess, pending.active)
		if err := c.onBoundary(sess); err != nil {
			logger.Warn("dsh-plan-mode: failed to append selected plan mode at step start: " + err.Error())
			return decision
		}
		if !pending.narrate || narration == nil {
			return decision
		}
		messages := make([]llm.Message, 0, len(decision.Messages)+1)
		messages = append(messages, decision.Messages...)
		messages = append(messages, *narration)
		decision.Messages = messages
		return decision
	})
}

// RegisterSection registers the `plan:policy` prompt section for one
// agent's scope: the deployment guidance while plan mode is active or a
// selection targets it, empty otherwise. Go adaptation: the section text
// provider receives no agent, so the host registers one scoped section per
// agent session.
func (c *Controller) RegisterSection(sp *systemprompt.SystemPrompt, scopeKey scope.ScopeKey, sess *session.Session) (func(), error) {
	return sp.Section(scopeKey, systemprompt.PromptSection{
		Name:  SectionName,
		Order: systemprompt.OrderPlanPolicy,
		TextProvider: func(context systemprompt.AssembleContext) string {
			return c.SectionText(sess)
		},
	})
}

// SectionText is the section's dynamic text. The gate is the official
// `pending?.active ?? fold`: a pending selection — either direction —
// REPLACES the folded state outright, so a pending exit immediately hides
// the section even while the log still reads active; only a session with no
// pending selection falls back to the fold.
func (c *Controller) SectionText(sess *session.Session) string {
	c.mu.Lock()
	pending, hasPending := c.pending[sess]
	c.mu.Unlock()
	if hasPending {
		if pending.active {
			return c.section
		}
		return ""
	}
	if FoldPlanMode(sess.Events(), -1) {
		return c.section
	}
	return ""
}

// onBoundary appends one pending selection before the next request
// assembly.
func (c *Controller) onBoundary(sess *session.Session) error {
	c.mu.Lock()
	pending, hasPending := c.pending[sess]
	c.mu.Unlock()
	if !hasPending {
		return nil
	}
	if pending.active == FoldPlanMode(sess.Events(), -1) {
		c.mu.Lock()
		delete(c.pending, sess)
		c.mu.Unlock()
		return nil
	}
	if _, err := sess.Append(EventPlanMode, PlanModeData{Active: pending.active}, nil); err != nil {
		return err
	}
	// Delete only after append succeeds so a later accepted in-turn
	// pre-step can retry a failed durable write.
	c.mu.Lock()
	delete(c.pending, sess)
	c.mu.Unlock()
	return nil
}

// narration builds a user-switch notice when the last logged header
// described the other mode.
func (c *Controller) narration(sess *session.Session, target bool) *llm.Message {
	told, has := PlanModeAtLastHeader(sess.Events())
	if !has || told == target {
		return nil
	}
	text := "The user switched this session back to the default mode."
	if target {
		text = "The user switched this session to plan mode."
	}
	message := llm.NewUserMessage(
		[]llm.ContentBlock{{Type: llm.BlockText, Text: text}},
		// The narration is already one sentence, so it is its own summary.
		llm.MessageSource{Kind: llm.SourcePlugin, Plugin: pluginName, Form: llm.FormNotice, Summary: text},
	)
	return &message
}

// planUnitState is the projection unit state: the logged mode, the latest
// successful `/plan` selection not yet resolved by a `plan/mode` commit,
// and an execution whose paired command settlement has not landed. Plain
// JSON (persisted-cache precondition).
type planUnitState struct {
	Active bool `json:"active"`
	// Wanted is the selection's target mode; nil when no selection is
	// outstanding.
	Wanted *bool `json:"wanted"`
	// Running is the latest plan command awaiting its paired settlement.
	Running *runningCommand `json:"running"`
}

type runningCommand struct {
	CommandID string `json:"commandId"`
	Wanted    bool   `json:"wanted"`
}

// PlanProjection is the plan projection's wire value. Active is the logged
// state in force; Pending is true while a logged `/plan` selection targets
// a state other than `active`, has not failed through its paired command
// settlement, and no later `plan/mode` event has recorded that state.
type PlanProjection struct {
	Active  bool `json:"active"`
	Pending bool `json:"pending"`
}

// ProjectionDefinition builds the `plan` projection unit: a pure event fold
// serving clients the whole {active, pending} value. `command/run` records
// the user's logged /plan selection, its paired settlement keeps only
// successful selections, and `plan/mode` records that selection and clears
// it. Pending is thereby a pure replay quantity: host restarts, other tabs,
// and cold reads all recover it from the log alone.
func ProjectionDefinition() projection.Definition {
	return projection.Unit[planUnitState]{
		Key:          "plan",
		StateVersion: 2,
		Init: func(header session.SessionHeader) planUnitState {
			return planUnitState{Active: false, Wanted: nil, Running: nil}
		},
		Apply: func(current planUnitState, event session.Event) (planUnitState, bool) {
			switch event.Type {
			case "command/run":
				var data commandRunData
				if err := json.Unmarshal(event.Data, &data); err != nil || data.Name != "plan" || data.Args == nil {
					return current, false
				}
				wanted := strings.TrimSpace(*data.Args) != "off"
				return planUnitState{Active: current.Active, Wanted: current.Wanted,
					Running: &runningCommand{CommandID: data.CommandID, Wanted: wanted}}, true
			case "command/done":
				var done commandDoneData
				if err := json.Unmarshal(event.Data, &done); err != nil || current.Running == nil || done.CommandID != current.Running.CommandID {
					return current, false
				}
				var next planUnitState
				if done.Kind == "success" && current.Running.Wanted != current.Active {
					wanted := current.Running.Wanted
					next = planUnitState{Active: current.Active, Wanted: &wanted, Running: nil}
				} else {
					next = planUnitState{Active: current.Active, Wanted: nil, Running: nil}
				}
				return next, true
			case EventPlanMode:
				var data PlanModeData
				if err := json.Unmarshal(event.Data, &data); err != nil {
					return current, false
				}
				return planUnitState{Active: data.Active, Wanted: nil, Running: current.Running}, true
			default:
				return current, false
			}
		},
		View: func(current planUnitState) any {
			wanted := current.Wanted
			if current.Running != nil {
				value := current.Running.Wanted
				wanted = &value
			}
			pending := wanted != nil && *wanted != current.Active
			return PlanProjection{Active: current.Active, Pending: pending}
		},
		DecodeState: func(raw json.RawMessage) (planUnitState, error) {
			var decoded planUnitState
			if err := json.Unmarshal(raw, &decoded); err != nil {
				return planUnitState{}, fmt.Errorf("plan unit state: %w", err)
			}
			return decoded, nil
		},
	}.Definition()
}

// commandRunData is the `command/run` payload subset the fold reads; the
// commands capability owns the full vocabulary.
type commandRunData struct {
	Name      string  `json:"name"`
	CommandID string  `json:"commandId"`
	Args      *string `json:"args"`
}

// commandDoneData is the `command/done` payload subset the fold reads.
type commandDoneData struct {
	CommandID string `json:"commandId"`
	Kind      string `json:"kind"`
}

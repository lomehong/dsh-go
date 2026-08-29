// Completion-notice delivery: unreported settlements reach the owning agent
// injected into a busy owner's next-step inbox, or opening a turn on an idle
// one under the default wakeup delivery, bounded per owner by a wake budget
// that any user-authored input refills.
package toolsjobs

import (
	"sync"

	"dshgo/agent"
	"dshgo/jobs"
	"dshgo/llm"
)

// PluginName is the cordis plugin name this delivery belongs to.
const PluginName = "tool-jobs"

// NoticeForm is the completion notice's plugin context form.
const NoticeForm = "notice"

// AgentResolver maps a registry owner record to the live agent carrying the
// loop-owned driver. The official listener receives the Agent itself; the Go
// registry deliberately keeps its owner face storage-only, so the assembler
// supplies this resolver.
type AgentResolver func(owner jobs.Owner) (*agent.Agent, bool)

// CompletionDeliverer owns one attached delivery arm. Wake-budget bookkeeping
// is keyed by the exact *agent.Agent, so a same-session replacement starts
// with a full budget.
type CompletionDeliverer struct {
	delivery   string
	wakeBudget int

	mu        sync.Mutex
	spent     map[*agent.Agent]int
	claimed   map[*agent.Agent]func()
	detach    func()
	detachAll func()
}

// AttachDelivery registers the completion listener on the registry's global
// layer — the registry routes each settlement to the listeners its owner's
// scope chain reaches, so this arm owns delivery, not the choice of whom to
// deliver to — and, under wakeup delivery, arms the budget refill on
// human-input claims. The returned disposer detaches everything.
func AttachDelivery(registry *jobs.LocalRegistry, resolveOwner AgentResolver, config Config) (*CompletionDeliverer, func(), error) {
	resolved, err := ResolveConfig(config)
	if err != nil {
		return nil, nil, err
	}
	if resolveOwner == nil {
		resolveOwner = func(jobs.Owner) (*agent.Agent, bool) { return nil, false }
	}
	d := &CompletionDeliverer{
		delivery:   resolved.CompletionDelivery,
		wakeBudget: resolved.MaxConsecutiveWakes,
		spent:      map[*agent.Agent]int{},
		claimed:    map[*agent.Agent]func(){},
	}
	detachDone := registry.OnJobDoneIn(nil, func(snapshot jobs.Snapshot, owner jobs.Owner) {
		d.deliver(snapshot, owner, resolveOwner)
	})
	d.mu.Lock()
	d.detach = detachDone
	d.mu.Unlock()
	detachAll := func() {
		d.mu.Lock()
		claimed := d.claimed
		d.claimed = map[*agent.Agent]func(){}
		detachDone := d.detach
		d.detach = nil
		d.mu.Unlock()
		for _, fn := range claimed {
			fn()
		}
		if detachDone != nil {
			detachDone()
		}
	}
	d.detachAll = detachAll
	return d, detachAll, nil
}

// deliver routes one unreported settlement to its owner. A busy owner is
// injected: the notice waits in its next-step inbox, which the turn cannot
// close over, so jobs settling together cost one step. An idle owner is
// woken instead, because an unclaimed notice is a completion the model never
// learns about. Either way, disposal before the claim discards it with the
// owner, and teardown settlements arrive reported.
func (d *CompletionDeliverer) deliver(snapshot jobs.Snapshot, owner jobs.Owner, resolveOwner AgentResolver) {
	if snapshot.Reported {
		return
	}
	live, ok := resolveOwner(owner)
	if !ok || live == nil {
		return
	}
	d.refillOnClaims(live)
	message := llm.NewUserMessage(
		[]llm.ContentBlock{{Type: llm.BlockText, Text: FitCompletionNotice(snapshot)}},
		llm.MessageSource{
			Kind:    llm.SourcePlugin,
			Plugin:  PluginName,
			Form:    NoticeForm,
			Summary: CompletionSummary(snapshot),
		},
	)
	d.mu.Lock()
	spent := d.spent[live]
	d.mu.Unlock()
	if d.delivery == DeliveryWakeup && live.Status() == agent.AgentIdle && spent < d.wakeBudget {
		d.mu.Lock()
		d.spent[live] = spent + 1
		d.mu.Unlock()
		if driver := live.Driver(); driver != nil {
			driver.Followup(message)
			return
		}
	}
	// Busy owner, exhausted budget, or quiet delivery: park the notice in
	// the next-step inbox without waking the driver.
	if driver := live.Driver(); driver != nil {
		driver.Inject(message)
		return
	}
	_ = live.Inbox.Append(agent.InboxNextStep, message)
}

// SpentWakes reports the wake budget one agent has spent — test seam.
func (d *CompletionDeliverer) SpentWakes(live *agent.Agent) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.spent[live]
}

// refillOnClaims registers the budget-refill listener on one agent's bus.
// Claiming is the point the human's input actually enters a step; a notice
// this arm itself queued must not refill the budget it just spent, so only
// user-kind claims refill.
func (d *CompletionDeliverer) refillOnClaims(live *agent.Agent) {
	if d.delivery != DeliveryWakeup {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.claimed[live]; ok {
		return
	}
	detach := live.Events().OnEmit(agent.EventInboxClaimed, live.Scope, func(payload any) error {
		claimed, ok := payload.(agent.AgentClaimedPayload)
		if !ok || claimed.Message.Source.Kind != llm.SourceUser {
			return nil
		}
		d.mu.Lock()
		delete(d.spent, live)
		d.mu.Unlock()
		return nil
	})
	d.claimed[live] = detach
}

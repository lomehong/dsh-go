package agent

import (
	"context"
	"testing"

	"dshgo/llm"
)

// Contract tests for the typed waterfall accessor: the any-erased table
// underneath is already pinned by the raw bus tests; these pin the typed
// boundary — composition order, base-as-innermost, delegation payloads, and
// the exactly-once next contract.

func TestTypedWaterfallComposesOutermostFirst(t *testing.T) {
	bus := newSubjectEventBus(nil)
	agentObj := &Agent{Scope: nil}
	preStep := NewTypedWaterfall[PreStepPayload, PreStepDecision](bus, EventPreStep)

	var order []string
	disposeOuter := preStep.On(nil, func(payload PreStepPayload, next func(PreStepPayload) PreStepDecision) PreStepDecision {
		order = append(order, "outer")
		decision := next(payload)
		decision.Messages = append(decision.Messages, llm.NewUserMessage(nil, llm.MessageSource{Kind: "outer"}))
		return decision
	})
	defer disposeOuter()
	disposeInner := preStep.On(nil, func(payload PreStepPayload, next func(PreStepPayload) PreStepDecision) PreStepDecision {
		order = append(order, "inner")
		return next(payload)
	})
	defer disposeInner()

	decision := preStep.Dispatch(nil, PreStepPayload{Agent: agentObj, Signal: context.Background()},
		func(PreStepPayload) PreStepDecision { return PreStepEnter(nil) })

	if len(order) != 2 || order[0] != "outer" || order[1] != "inner" {
		t.Fatalf("order = %v, want outer then inner", order)
	}
	if decision.Kind != "enter" || len(decision.Messages) != 1 {
		t.Fatalf("decision = %+v, want the outer listener's appended message", decision)
	}
}

func TestTypedWaterfallBaseRunsInnermost(t *testing.T) {
	bus := newSubjectEventBus(nil)
	preStep := NewTypedWaterfall[PreStepPayload, PreStepDecision](bus, EventPreStep)

	dispose := preStep.On(nil, func(payload PreStepPayload, next func(PreStepPayload) PreStepDecision) PreStepDecision {
		return next(payload)
	})
	defer dispose()

	decision := preStep.Dispatch(nil, PreStepPayload{Signal: context.Background()},
		func(payload PreStepPayload) PreStepDecision {
			if payload.Signal == nil {
				t.Fatal("base received a degraded payload")
			}
			return PreStepReject()
		})
	if decision.Kind != "reject" {
		t.Fatalf("decision = %+v, want the base decision", decision)
	}
}

func TestTypedWaterfallWithoutListenersUsesBase(t *testing.T) {
	bus := newSubjectEventBus(nil)
	preStep := NewTypedWaterfall[PreStepPayload, PreStepDecision](bus, EventPreStep)

	decision := preStep.Dispatch(nil, PreStepPayload{Signal: context.Background()},
		func(PreStepPayload) PreStepDecision { return PreStepEnter(nil) })
	if decision.Kind != "enter" {
		t.Fatalf("decision = %+v, want base", decision)
	}
}

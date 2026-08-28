package tokenmeter

import (
	"encoding/json"
	"fmt"
	"sync"

	"dshgo/llm"
	"dshgo/session"
)

// measurementAnchor holds the raw anchor facts captured at the latest
// successful call; the baseline is derived per measurement so the anchored
// surface reprices under the same route pricing as the current surface it is
// compared with.
type measurementAnchor struct {
	header *session.EpochHeader
	// nodes is the surface snapshot the anchored request was derived from.
	nodes []MeterSurfaceNode
	// assistantTokens is the fixed-heuristic price of the call's provider
	// output.
	assistantTokens int64
	// usage is the provider usage of the call, when it reported one under a
	// known header.
	usage *llm.TokenUsage
}

// stepStartMark records the open step bracket and the surface it opened
// over.
type stepStartMark struct {
	turn  int64
	step  int64
	nodes []MeterSurfaceNode
}

// replayState is one session's isolated fold.
type replayState struct {
	consumedEvents int64
	header         *session.EpochHeader
	surface        []MeterSurfaceNode
	stepStart      *stepStartMark
	anchor         *measurementAnchor
}

// Meter is the replay owner for one service-wide estimator and isolated
// per-session folds. Go adaptations: the WeakMap becomes a keyed map and the
// optional `llm` route-pricing lookup becomes the routePricing seam the
// composition supplies.
type Meter struct {
	mu     sync.Mutex
	states map[*session.Session]*replayState
	// routePricing resolves the routed model's image pricing, when the
	// routed adapter declares one.
	routePricing func(provider string, model string) ImageRequestPricing
}

// NewMeter builds the meter. routePricing may be nil: every surface then
// keeps its fixed heuristic price.
func NewMeter(routePricing func(provider string, model string) ImageRequestPricing) *Meter {
	return &Meter{
		states:       map[*session.Session]*replayState{},
		routePricing: routePricing,
	}
}

// Measure current request pressure and surface through the durable tail.
//
// The effective envelope's routed provider/model selects the request-image
// pricing every node is priced under. Provider usage is reused only when the
// latest successful call's canonical request envelope matches requestHeader
// and its total is no lower than that call's full route-priced anchor;
// otherwise the complete envelope and surface are repriced. requestHeader
// replaces the latest logged envelope for pressure and node pricing; the
// node set always describes the current session surface.
func (m *Meter) Measure(sess *session.Session, requestHeader *session.EpochHeader) (Measurement, error) {
	state, err := m.Sync(sess)
	if err != nil {
		return Measurement{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var header *session.EpochHeader
	if requestHeader != nil {
		canonical := session.CanonicalHeader(*requestHeader)
		header = &canonical
	} else {
		header = state.header
	}
	var pricing ImageRequestPricing
	if header != nil && m.routePricing != nil {
		pricing = m.routePricing(header.Config.Provider, header.Config.Model)
	}
	surface, err := PriceSurface(state.surface, pricing)
	if err != nil {
		return Measurement{}, err
	}
	anchor := state.anchor

	var baseline Baseline
	var surfaceDeltaTokens int64
	switch {
	case anchor != nil && optionalHeaderEquals(anchor.header, header):
		// Matching headers share one route, so the anchored snapshot
		// reprices under the same pricing as the current surface and the
		// signed delta compares like with like.
		anchorPriced, err := PriceSurface(anchor.nodes, pricing)
		if err != nil {
			return Measurement{}, err
		}
		anchorSurfaceTokens := anchorPriced.SurfaceTokens + anchor.assistantTokens
		estimatedAnchorTokens := EstimateHeader(header) + anchorSurfaceTokens
		if anchor.usage != nil && usageTokens(*anchor.usage) >= estimatedAnchorTokens {
			usage := *anchor.usage
			baseline = Baseline{Kind: BaselineUsage, Tokens: usageTokens(usage), Usage: &usage}
		} else {
			baseline = Baseline{Kind: BaselineEstimated, Tokens: estimatedAnchorTokens}
		}
		surfaceDeltaTokens = surface.SurfaceTokens - anchorSurfaceTokens
	case header == nil && surface.SurfaceTokens == 0:
		baseline = Baseline{Kind: BaselineNone}
	default:
		baseline = Baseline{
			Kind:   BaselineEstimated,
			Tokens: EstimateHeader(header) + surface.SurfaceTokens,
		}
		surfaceDeltaTokens = 0
	}

	return Measurement{
		LogRevision:        state.consumedEvents,
		Baseline:           baseline,
		SurfaceDeltaTokens: surfaceDeltaTokens,
		TotalTokens:        maxInt64(baseline.Tokens + surfaceDeltaTokens),
		SurfaceTokens:      surface.SurfaceTokens,
		Nodes:              surface.Nodes,
	}, nil
}

// Sync catches one session's fold up to the current durable tail. A
// malformed event fails every subsequent call identically until the session
// is fixed — the fold never half-applies.
func (m *Meter) Sync(sess *session.Session) (*replayState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.states[sess]
	if !ok {
		state = &replayState{}
		m.states[sess] = state
	}
	events := sess.Events()
	for state.consumedEvents < int64(len(events)) {
		event := events[state.consumedEvents]
		if err := m.foldEvent(state, event, events); err != nil {
			return nil, err
		}
		state.consumedEvents++
	}
	return state, nil
}

// foldEvent runs every fallible step — header decode, bracket matching,
// surface plan, and anchor validation — before mutating replay state, so a
// malformed event remains unread on every retry instead of half-applying.
func (m *Meter) foldEvent(state *replayState, event session.Event, events []session.Event) error {
	var nextHeader *session.EpochHeader
	if state.header != nil {
		copied := *state.header
		nextHeader = &copied
	}
	var nextStepStart *stepStartMark
	if state.stepStart != nil {
		mark := *state.stepStart
		nextStepStart = &mark
	}
	var nextAnchor *measurementAnchor
	if state.anchor != nil {
		anchor := *state.anchor
		nextAnchor = &anchor
	}

	switch event.Type {
	case session.EventRequestHeader:
		var data session.RequestHeaderData
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return fmt.Errorf("token meter: request/header at seq %d: %w", event.Seq, err)
		}
		canonical := session.CanonicalHeader(data.Header)
		nextHeader = &canonical
	case session.EventStepStart:
		if state.stepStart != nil {
			return fmt.Errorf(
				"token meter: step/start at seq %d arrived before turn %d/step %d ended",
				event.Seq, state.stepStart.turn, state.stepStart.step)
		}
		var data session.StepStartData
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return fmt.Errorf("token meter: step/start at seq %d: %w", event.Seq, err)
		}
		nextStepStart = &stepStartMark{turn: data.Turn, step: data.Step, nodes: append([]MeterSurfaceNode{}, state.surface...)}
	case session.EventStepEnd:
		var data session.StepEndData
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return fmt.Errorf("token meter: step/end at seq %d: %w", event.Seq, err)
		}
		if state.stepStart == nil || state.stepStart.turn != data.Turn || state.stepStart.step != data.Step {
			return fmt.Errorf("token meter: step/end at seq %d has no matching step/start event", event.Seq)
		}
		nextStepStart = nil
	}

	var plan *SurfaceTokenPlan
	if session.IsSurfaceEventType(event.Type) {
		validated, err := PlanSurfaceTokens(state.surface, event)
		if err != nil {
			return err
		}
		plan = &validated
	}

	if event.Type == session.EventAssistantMsg {
		if state.stepStart == nil {
			return fmt.Errorf("token meter: assistant/message at seq %d has no matching step/start event", event.Seq)
		}
		stepStart := state.stepStart
		var data session.AssistantMessageData
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return fmt.Errorf("token meter: assistant/message at seq %d: %w", event.Seq, err)
		}
		if stepStart.turn != data.Turn || stepStart.step != data.Step {
			return fmt.Errorf("token meter: assistant/message at seq %d has no matching step/start event", event.Seq)
		}
		// assistant/message is surface-mandatory at every append/seed
		// boundary, so the plan price is its durable price.
		eventTokens := plan.Tokens
		if data.Usage != nil && nextHeader != nil {
			assistantTokens, err := estimateProviderAssistant(event, events, eventTokens)
			if err != nil {
				return err
			}
			usage := *data.Usage
			nextAnchor = &measurementAnchor{
				header:          nextHeader,
				nodes:           stepStart.nodes,
				assistantTokens: assistantTokens,
				usage:           &usage,
			}
		} else {
			nextAnchor = &measurementAnchor{
				header:          nextHeader,
				nodes:           stepStart.nodes,
				assistantTokens: eventTokens,
			}
		}
	}

	state.header = nextHeader
	state.stepStart = nextStepStart
	if plan != nil {
		CommitSurfaceTokens(&state.surface, *plan)
	}
	state.anchor = nextAnchor
	return nil
}

// estimateProviderAssistant reassembles provider output from the exact
// cited chunk seqs for a usage anchor. Missing legacy source seqs
// conservatively treat the durable output as the provider output; an
// explicit empty list prices a known empty stream.
func estimateProviderAssistant(event session.Event, events []session.Event, durableEventTokens int64) (int64, error) {
	sourceSeqs := event.SourceEventSeqs
	if sourceSeqs == nil {
		return durableEventTokens, nil
	}
	var messageData session.AssistantMessageData
	if err := json.Unmarshal(event.Data, &messageData); err != nil {
		return 0, fmt.Errorf("token meter: assistant/message at seq %d: %w", event.Seq, err)
	}
	assembler := llm.NewBlockAssembler()
	seen := map[int64]bool{}
	for _, seq := range sourceSeqs {
		if seq >= event.Seq {
			return 0, fmt.Errorf("token meter: assistant/message at seq %d source seq %d is not earlier", event.Seq, seq)
		}
		if seen[seq] {
			return 0, fmt.Errorf("token meter: assistant/message at seq %d repeats source seq %d", event.Seq, seq)
		}
		seen[seq] = true
		if int(seq) >= len(events) {
			return 0, fmt.Errorf("token meter: assistant/message at seq %d source seq %d is not earlier", event.Seq, seq)
		}
		sourceEvent := events[seq]
		if sourceEvent.Type != session.EventAssistantChunk {
			return 0, fmt.Errorf("token meter: assistant/message at seq %d source seq %d is not assistant/chunk", event.Seq, seq)
		}
		var chunkData struct {
			Turn  int64           `json:"turn"`
			Step  int64           `json:"step"`
			Chunk llm.StreamChunk `json:"chunk"`
		}
		if err := json.Unmarshal(sourceEvent.Data, &chunkData); err != nil {
			return 0, fmt.Errorf("token meter: assistant/message at seq %d source seq %d: %w", event.Seq, seq, err)
		}
		if chunkData.Turn != messageData.Turn || chunkData.Step != messageData.Step {
			return 0, fmt.Errorf("token meter: assistant/message at seq %d source seq %d belongs to another step", event.Seq, seq)
		}
		assembler.Push(chunkData.Chunk)
	}
	blocks := assembler.Blocks()
	if len(blocks) == 0 {
		return 0, nil
	}
	return EstimateContent(blocks) + RoleOverhead, nil
}

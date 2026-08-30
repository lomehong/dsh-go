package tokenmeter

import (
	"encoding/json"
	"fmt"

	"dshgo/llm"
	"dshgo/session"
	"dshgo/session/projection"
)

// jsonUnmarshal is decodeEventPayload's decode primitive.
func jsonUnmarshal(data []byte, target any) error { return json.Unmarshal(data, target) }

// The durable event vocabulary of the official @deepseek-ai/dsh-llm-retry
// plugin. The Go retry plugin is not ported yet — until it lands, no
// producer writes these types; the folds below read them so the projection
// units are already correct over logs the retry plugin will write.
const (
	// EventLlmRetry is recorded before one provider-routed retry wait.
	EventLlmRetry = "llm/retry"
	// EventLlmRetryStarted is recorded after one retry delay completes.
	EventLlmRetryStarted = "llm/retry-started"
)

// TokenUsageProjection is the durable cumulative provider usage for a
// complete session log. The four buckets are disjoint; reasoning tokens are
// already included in OutputTokens and are not accumulated again.
type TokenUsageProjection struct {
	UncachedInputTokens int64 `json:"uncachedInputTokens"`
	OutputTokens        int64 `json:"outputTokens"`
	CacheReadTokens     int64 `json:"cacheReadTokens"`
	CacheWriteTokens    int64 `json:"cacheWriteTokens"`
}

// ContextPressureProjection is the approximate context occupancy for a
// status display. The fields, when present, are deliberately NOT one atomic
// request observation: each is a last-wins record of a different moment.
// Switching models can therefore pair a fresh capacity with the previous
// route's pressure until the next request reports usage — an intentional
// trade: the value is a user-facing reference, not a billing or gating
// input.
type ContextPressureProjection struct {
	// PressureTokens is the provider-reported prompt size of the most
	// recent request: uncached input plus cache reads and writes. Response
	// output is excluded, so this does not grow as the current turn
	// streams. Absent until a provider reports usage.
	PressureTokens *int64 `json:"pressureTokens,omitempty"`
	// ProjectedTokens is what the NEXT request's prompt would cost:
	// PressureTokens plus the heuristic repricing of everything the
	// surface gained or lost since that sample. Only the delta is
	// estimated, so the figure stays anchored to the provider while still
	// reacting the moment a compaction shadows a span. Absent until a
	// provider reports usage.
	ProjectedTokens *int64 `json:"projectedTokens,omitempty"`
	// ContextWindow is the newest recorded route capacity; absent when no
	// adapter advertised one.
	ContextWindow *int64 `json:"contextWindow,omitempty"`
}

// ContextBreakdownProjection is the heuristic composition of the next
// request's context: what the prompt is made of, not what it costs. All
// three figures use the meter's fixed density estimate, so they will not
// sum to the provider-anchored ProjectedTokens. Present these as
// approximations of composition, never as a total.
type ContextBreakdownProjection struct {
	// SystemTokens is the heuristic tokens of the newest request
	// envelope's system prompt; 0 before any request.
	SystemTokens int64 `json:"systemTokens"`
	// ToolsTokens is the heuristic tokens of the newest request envelope's
	// tool schemas; 0 before any request.
	ToolsTokens int64 `json:"toolsTokens"`
	// MessageTokens is the heuristic tokens of the current model-visible
	// conversation surface.
	MessageTokens int64 `json:"messageTokens"`
}

// tokenUsageSample is the single replacement slot: the usage one attempt
// reported, kept so a repeated sample replaces instead of double counts.
type tokenUsageSample struct {
	Turn    int64                `json:"turn"`
	Step    int64                `json:"step"`
	Buckets TokenUsageProjection `json:"buckets"`
}

// tokenUsageState is the token-usage unit's whole state: O(1) by
// construction.
type tokenUsageState struct {
	Totals TokenUsageProjection `json:"totals"`
	Last   *tokenUsageSample    `json:"last"`
}

// contextPressureState is the context-occupancy unit's whole state. The
// running surface total rides the shadow-price fold, so the state stays a
// fixed handful of numbers.
type contextPressureState struct {
	ContextWindow        *int64            `json:"contextWindow,omitempty"`
	PressureTokens       *int64            `json:"pressureTokens,omitempty"`
	SurfaceTokens        int64             `json:"surfaceTokens"`
	SampledSurfaceTokens *int64            `json:"sampledSurfaceTokens,omitempty"`
	Claim                *ShadowPriceClaim `json:"claim,omitempty"`
}

// contextBreakdownState is the context-composition unit's whole state.
type contextBreakdownState struct {
	SystemTokens  int64             `json:"systemTokens"`
	ToolsTokens   int64             `json:"toolsTokens"`
	MessageTokens int64             `json:"messageTokens"`
	Claim         *ShadowPriceClaim `json:"claim,omitempty"`
}

// decodeJSONState is the shared DecodeState: the persisted row value must
// be the unit state's exact plain-JSON shape.
func decodeJSONState[S any](raw json.RawMessage) (S, error) {
	var state S
	if err := json.Unmarshal(raw, &state); err != nil {
		return state, err
	}
	return state, nil
}

// bucketsFrom detaches one provider report into the disjoint projection
// buckets.
func bucketsFrom(usage llm.TokenUsage) TokenUsageProjection {
	return TokenUsageProjection{
		UncachedInputTokens: usage.InputTokens,
		OutputTokens:        usage.OutputTokens,
		CacheReadTokens:     optionalTokens(usage.CacheReadTokens),
		CacheWriteTokens:    optionalTokens(usage.CacheWriteTokens),
	}
}

func optionalTokens(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

// addReplacing folds one attempt's replacement: the previous sample for the
// same attempt subtracts before the next adds.
func addReplacing(totals TokenUsageProjection, previous *TokenUsageProjection, next TokenUsageProjection) TokenUsageProjection {
	nextTotals := totals
	if previous != nil {
		nextTotals.UncachedInputTokens -= previous.UncachedInputTokens
		nextTotals.OutputTokens -= previous.OutputTokens
		nextTotals.CacheReadTokens -= previous.CacheReadTokens
		nextTotals.CacheWriteTokens -= previous.CacheWriteTokens
	}
	nextTotals.UncachedInputTokens += next.UncachedInputTokens
	nextTotals.OutputTokens += next.OutputTokens
	nextTotals.CacheReadTokens += next.CacheReadTokens
	nextTotals.CacheWriteTokens += next.CacheWriteTokens
	return nextTotals
}

// eventUsage is the usage a chunk or finalized message reports for its
// step, with the attempt identity.
type eventUsage struct {
	Turn    int64
	Step    int64
	Buckets TokenUsageProjection
}

// projectionFoldError marks a failure inside an otherwise interesting
// event: a projection unit cannot return an error, so decode failures and
// shadow-price contract violations fail loud through the fold.
type projectionFoldError struct {
	event session.Event
	err   error
}

func (e projectionFoldError) Error() string {
	return fmt.Sprintf("token meter projection: %s at seq %d: %v", e.event.Type, e.event.Seq, e.err)
}

// usageOf decodes the usage an event reports, or nil when it reports none.
func usageOf(event session.Event) *eventUsage {
	switch event.Type {
	case session.EventAssistantChunk:
		var data struct {
			Turn  int64           `json:"turn"`
			Step  int64           `json:"step"`
			Chunk llm.StreamChunk `json:"chunk"`
		}
		if err := decodeEventPayload(event, &data); err != nil {
			panic(projectionFoldError{event: event, err: err})
		}
		if data.Chunk.Type != llm.ChunkUsage || data.Chunk.Usage == nil {
			return nil
		}
		return &eventUsage{Turn: data.Turn, Step: data.Step, Buckets: bucketsFrom(*data.Chunk.Usage)}
	case session.EventAssistantMsg:
		var data session.AssistantMessageData
		if err := decodeEventPayload(event, &data); err != nil {
			panic(projectionFoldError{event: event, err: err})
		}
		if data.Usage == nil {
			return nil
		}
		return &eventUsage{Turn: data.Turn, Step: data.Step, Buckets: bucketsFrom(*data.Usage)}
	}
	return nil
}

// TokenUsageUnit is the token-meter usage projection unit: provider-reported
// usage accumulated across the complete durable log.
//
// Usage chunks provide an early sample that survives a later request
// failure; an assistant message provides the final sample for the same
// attempt. A repeated sample replaces that attempt's earlier value instead
// of double counting it, while `llm/retry-started` closes the replacement
// slot so the retried attempt adds to the total. The single `last` slot
// relies on the session-log invariant that usage reports for one attempt
// are adjacent.
func TokenUsageUnit() projection.Definition {
	unit := projection.Unit[tokenUsageState]{
		Key:          "tokenUsage",
		StateVersion: 2,
		Init: func(session.SessionHeader) tokenUsageState {
			return tokenUsageState{}
		},
		Apply: func(state tokenUsageState, event session.Event) (tokenUsageState, bool) {
			if event.Type == EventLlmRetryStarted {
				var data struct {
					Turn int64 `json:"turn"`
					Step int64 `json:"step"`
				}
				if err := decodeEventPayload(event, &data); err != nil {
					panic(projectionFoldError{event: event, err: err})
				}
				if state.Last != nil && state.Last.Turn == data.Turn && state.Last.Step == data.Step {
					return tokenUsageState{Totals: state.Totals}, true
				}
				return state, false
			}
			usage := usageOf(event)
			if usage == nil {
				return state, false
			}
			var previous *TokenUsageProjection
			if state.Last != nil && state.Last.Turn == usage.Turn && state.Last.Step == usage.Step {
				if state.Last.Buckets == usage.Buckets {
					return state, false
				}
				copied := state.Last.Buckets
				previous = &copied
			}
			return tokenUsageState{
				Totals: addReplacing(state.Totals, previous, usage.Buckets),
				Last:   &tokenUsageSample{Turn: usage.Turn, Step: usage.Step, Buckets: usage.Buckets},
			}, true
		},
		View: func(state tokenUsageState) any {
			return state.Totals
		},
		DecodeState: decodeJSONState[tokenUsageState],
	}
	return unit.Definition()
}

// ContextPressureUnit is the token-meter context-occupancy projection unit.
//
// Independent last-wins slots: the newest usage sample supplies the
// provider numerator, the newest `request/context` record the denominator.
// Both are whole values, so replay order alone decides the result and no
// cross-field consistency is claimed — the pair is explicitly not one
// atomic request observation.
//
// PressureTokens is prompt-side only, so it holds still while a turn
// streams and steps forward once the next request reports its usage.
// Because nothing but a request reports usage, it also cannot see a
// compaction: the fold therefore carries a running surface total alongside
// it and publishes ProjectedTokens — the sample plus the surface's signed
// movement since it was taken. The total rides FoldSurfaceTokens, so the
// state stays O(1) and a replacement shrinks it by its logged shadow price.
// A replacement without a claim preserves the previous total. A usage
// sample is stamped BEFORE the same event joins the surface, so an
// assistant/message anchors against the surface its own request saw.
func ContextPressureUnit() projection.Definition {
	unit := projection.Unit[contextPressureState]{
		Key:          "contextPressure",
		StateVersion: 4,
		Init: func(session.SessionHeader) contextPressureState {
			return contextPressureState{}
		},
		Apply: func(state contextPressureState, event session.Event) (contextPressureState, bool) {
			fold, err := FoldSurfaceTokens(state.Claim, event)
			if err != nil {
				panic(projectionFoldError{event: event, err: err})
			}
			next := state
			if event.Type == session.EventRequestCtx {
				var data session.RequestContext
				if err := decodeEventPayload(event, &data); err != nil {
					panic(projectionFoldError{event: event, err: err})
				}
				next.ContextWindow = data.ContextWindow
			}
			if usage := usageOf(event); usage != nil {
				pressureTokens := usage.Buckets.UncachedInputTokens + usage.Buckets.CacheReadTokens + usage.Buckets.CacheWriteTokens
				sampled := int64(0)
				if next.SampledSurfaceTokens != nil {
					sampled = *next.SampledSurfaceTokens
				}
				if next.PressureTokens == nil || *next.PressureTokens != pressureTokens || sampled != next.SurfaceTokens {
					pressure := pressureTokens
					sampledNow := next.SurfaceTokens
					next.PressureTokens = &pressure
					next.SampledSurfaceTokens = &sampledNow
				}
			}
			if fold.DeltaTokens != 0 {
				next.SurfaceTokens += fold.DeltaTokens
			}
			if state.Claim == nil && fold.Claim == nil {
				return next, next != state
			}
			next.Claim = fold.Claim
			return next, true
		},
		View: func(state contextPressureState) any {
			view := ContextPressureProjection{
				ContextWindow:  state.ContextWindow,
				PressureTokens: state.PressureTokens,
			}
			if state.PressureTokens != nil && state.SampledSurfaceTokens != nil {
				projected := *state.PressureTokens + state.SurfaceTokens - *state.SampledSurfaceTokens
				if projected < 0 {
					projected = 0
				}
				view.ProjectedTokens = &projected
			}
			return view
		},
		DecodeState: decodeJSONState[contextPressureState],
	}
	return unit.Definition()
}

// ContextBreakdownUnit is the token-meter context-composition projection
// unit.
//
// Envelope figures are last-wins per `request/header`; the message figure
// rides FoldSurfaceTokens — the same O(1) fold the occupancy projection
// uses — so fully metered logs equal the sum of the surface nodes'
// heuristic prices at every event boundary and compaction shrinks the
// figure by its logged shadow price; the route-priced meter surface
// deliberately diverges by the routed model's image repricing. A
// replacement without a claim preserves the previous total. The state is a
// fixed handful of numbers, so the persisted checkpoint stays O(1) over the
// session's life.
func ContextBreakdownUnit() projection.Definition {
	unit := projection.Unit[contextBreakdownState]{
		Key:          "contextBreakdown",
		StateVersion: 2,
		Init: func(session.SessionHeader) contextBreakdownState {
			return contextBreakdownState{}
		},
		Apply: func(state contextBreakdownState, event session.Event) (contextBreakdownState, bool) {
			fold, err := FoldSurfaceTokens(state.Claim, event)
			if err != nil {
				panic(projectionFoldError{event: event, err: err})
			}
			systemTokens := state.SystemTokens
			toolsTokens := state.ToolsTokens
			if event.Type == session.EventRequestHeader {
				var data session.RequestHeaderData
				if err := decodeEventPayload(event, &data); err != nil {
					panic(projectionFoldError{event: event, err: err})
				}
				canonical := session.CanonicalHeader(data.Header)
				systemTokens = EstimateSystemTokens(&canonical)
				toolsTokens = EstimateToolsTokens(&canonical)
			}
			if systemTokens == state.SystemTokens &&
				toolsTokens == state.ToolsTokens &&
				fold.DeltaTokens == 0 &&
				fold.Claim == nil &&
				state.Claim == nil {
				return state, false
			}
			return contextBreakdownState{
				SystemTokens:  systemTokens,
				ToolsTokens:   toolsTokens,
				MessageTokens: state.MessageTokens + fold.DeltaTokens,
				Claim:         fold.Claim,
			}, true
		},
		View: func(state contextBreakdownState) any {
			return ContextBreakdownProjection{
				SystemTokens:  state.SystemTokens,
				ToolsTokens:   state.ToolsTokens,
				MessageTokens: state.MessageTokens,
			}
		},
		DecodeState: decodeJSONState[contextBreakdownState],
	}
	return unit.Definition()
}

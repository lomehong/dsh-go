package tokenmeter

import (
	"encoding/json"
	"math"

	"dshgo/llm"
	"dshgo/session"
)

// This file ports @deepseek-ai/dsh-token-meter/turn-usage: exact
// provider-reported token accounting for one completed Turn, folded from
// the turn-local durable attempt lifecycle.

// TurnTokenUsageRoute is one provider/model route that contributed a billed
// request attempt.
type TurnTokenUsageRoute struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// TurnTokenUsage is the exact provider-reported token accounting for every
// attempt in one completed Turn.
type TurnTokenUsage struct {
	// UncachedInputTokens is the sum of uncached prompt input across all
	// attempts.
	UncachedInputTokens int64 `json:"uncachedInputTokens"`
	OutputTokens        int64 `json:"outputTokens"`
	// TotalTokens is the exact aggregate prompt plus output total across
	// all attempts.
	TotalTokens int64 `json:"totalTokens"`
	// CacheReadTokens is present only when every attempt reported the
	// bucket.
	CacheReadTokens *int64 `json:"cacheReadTokens,omitempty"`
	// CacheWriteTokens is present only when every attempt reported the
	// bucket.
	CacheWriteTokens *int64 `json:"cacheWriteTokens,omitempty"`
	// ReasoningTokens is the output subset, present only when every
	// attempt reported it.
	ReasoningTokens *int64 `json:"reasoningTokens,omitempty"`
	// Routes is present only when every billed attempt has provider/model
	// attribution.
	Routes []TurnTokenUsageRoute `json:"routes,omitempty"`
}

// normalizedAttempt is one attempt's validated usage. Optional buckets stay
// nil unless the provider reported them.
type normalizedAttempt struct {
	inputTokens      int64
	outputTokens     int64
	totalTokens      int64
	cacheReadTokens  *int64
	cacheWriteTokens *int64
	reasoningTokens  *int64
	route            *TurnTokenUsageRoute
}

// attemptStateKind is the attempt lifecycle fold's state.
type attemptStateKind int

const (
	attemptIdle attemptStateKind = iota
	attemptOpen
	attemptFinishClosed
	attemptSettled
)

type attemptState struct {
	kind      attemptStateKind
	settledBy string // "message" or "retry"; settled only
	turn      int64
	step      int64
	sample    *llm.TokenUsage
}

// isCount reports a non-negative safe integer (the count shape every
// validated bucket must carry).
func isCount(value int64) bool { return value >= 0 }

// safeSum adds counts, failing on int64 overflow (the safe-integer guard).
func safeSum(values []int64) (int64, bool) {
	var total int64
	for _, value := range values {
		if value > 0 && total > math.MaxInt64-value {
			return 0, false
		}
		total += value
	}
	return total, true
}

// messageRoute reads the provider/model attribution of an assistant
// message; either half absent leaves the attempt unattributed.
func messageRoute(message llm.Message) *TurnTokenUsageRoute {
	if message.Source.Provider == "" || message.Source.Model == "" {
		return nil
	}
	return &TurnTokenUsageRoute{Provider: message.Source.Provider, Model: message.Source.Model}
}

// normalizeUsage validates one provider report into an exact attempt.
// Incomplete usage, an unsafe count, or a contradictory exact total makes
// the attempt invalid.
func normalizeUsage(usage llm.TokenUsage, route *TurnTokenUsageRoute) (*normalizedAttempt, bool) {
	if !isCount(usage.InputTokens) || !isCount(usage.OutputTokens) {
		return nil, false
	}
	if usage.CacheReadTokens != nil && !isCount(*usage.CacheReadTokens) {
		return nil, false
	}
	if usage.CacheWriteTokens != nil && !isCount(*usage.CacheWriteTokens) {
		return nil, false
	}
	if usage.ReasoningTokens != nil && (!isCount(*usage.ReasoningTokens) || *usage.ReasoningTokens > usage.OutputTokens) {
		return nil, false
	}

	knownPrompt := []int64{usage.InputTokens}
	if usage.CacheReadTokens != nil {
		knownPrompt = append(knownPrompt, *usage.CacheReadTokens)
	}
	if usage.CacheWriteTokens != nil {
		knownPrompt = append(knownPrompt, *usage.CacheWriteTokens)
	}
	promptSum, ok := safeSum(knownPrompt)
	if !ok {
		return nil, false
	}

	var exactTotal int64
	if usage.TotalTokens != nil {
		if !isCount(*usage.TotalTokens) {
			return nil, false
		}
		exactPrompt := *usage.TotalTokens - usage.OutputTokens
		if !isCount(exactPrompt) || exactPrompt < promptSum {
			return nil, false
		}
		if usage.CacheReadTokens != nil && usage.CacheWriteTokens != nil && exactPrompt != promptSum {
			return nil, false
		}
		exactTotal = *usage.TotalTokens
	} else {
		if usage.CacheReadTokens == nil || usage.CacheWriteTokens == nil {
			return nil, false
		}
		derived, ok := safeSum([]int64{promptSum, usage.OutputTokens})
		if !ok {
			return nil, false
		}
		exactTotal = derived
	}

	return &normalizedAttempt{
		inputTokens:      usage.InputTokens,
		outputTokens:     usage.OutputTokens,
		totalTokens:      exactTotal,
		cacheReadTokens:  usage.CacheReadTokens,
		cacheWriteTokens: usage.CacheWriteTokens,
		reasoningTokens:  usage.ReasoningTokens,
		route:            route,
	}, true
}

// aggregateAttempts sums the attempts; an optional bucket is present only
// when every attempt reported it, and routes dedupe in first-seen order.
func aggregateAttempts(attempts []*normalizedAttempt) (*TurnTokenUsage, bool) {
	if len(attempts) == 0 {
		return nil, false
	}
	inputs := make([]int64, 0, len(attempts))
	outputs := make([]int64, 0, len(attempts))
	totals := make([]int64, 0, len(attempts))
	for _, attempt := range attempts {
		inputs = append(inputs, attempt.inputTokens)
		outputs = append(outputs, attempt.outputTokens)
		totals = append(totals, attempt.totalTokens)
	}
	inputSum, ok := safeSum(inputs)
	if !ok {
		return nil, false
	}
	outputSum, ok := safeSum(outputs)
	if !ok {
		return nil, false
	}
	totalSum, ok := safeSum(totals)
	if !ok {
		return nil, false
	}

	sumOptional := func(pick func(*normalizedAttempt) *int64) (*int64, bool) {
		values := make([]int64, 0, len(attempts))
		for _, attempt := range attempts {
			value := pick(attempt)
			if value == nil {
				return nil, true
			}
			values = append(values, *value)
		}
		sum, ok := safeSum(values)
		if !ok {
			return nil, false
		}
		return &sum, true
	}
	cacheRead, ok := sumOptional(func(a *normalizedAttempt) *int64 { return a.cacheReadTokens })
	if !ok {
		return nil, false
	}
	cacheWrite, ok := sumOptional(func(a *normalizedAttempt) *int64 { return a.cacheWriteTokens })
	if !ok {
		return nil, false
	}
	reasoning, ok := sumOptional(func(a *normalizedAttempt) *int64 { return a.reasoningTokens })
	if !ok {
		return nil, false
	}

	var routes []TurnTokenUsageRoute
	attributed := true
	for _, attempt := range attempts {
		if attempt.route == nil {
			attributed = false
			break
		}
	}
	if attributed {
		seen := map[TurnTokenUsageRoute]bool{}
		for _, attempt := range attempts {
			route := *attempt.route
			if !seen[route] {
				seen[route] = true
				routes = append(routes, route)
			}
		}
	}

	usage := &TurnTokenUsage{
		UncachedInputTokens: inputSum,
		OutputTokens:        outputSum,
		TotalTokens:         totalSum,
		CacheReadTokens:     cacheRead,
		CacheWriteTokens:    cacheWrite,
		ReasoningTokens:     reasoning,
		Routes:              routes,
	}
	return usage, true
}

// DeriveTurnTokenUsage folds one complete Turn's durable attempt lifecycle
// into exact token accounting.
//
// No attempt is inferred from a usage sample. Any missing lifecycle
// boundary, incomplete attempt usage, unsafe count, or contradictory exact
// total makes the whole disclosure unavailable.
//
// events are the Turn-local durable events from `turn/start` through
// `turn/end`. The result is nil when the accounting cannot be proven.
func DeriveTurnTokenUsage(events []session.Event) *TurnTokenUsage {
	state := attemptState{kind: attemptIdle}
	var attempts []*normalizedAttempt
	turn := int64(-1)
	sawStart := false
	sawEnd := false
	invalid := false

	closeOpen := func(route *TurnTokenUsageRoute) bool {
		if state.kind != attemptOpen || state.sample == nil {
			return false
		}
		normalized, ok := normalizeUsage(*state.sample, route)
		if !ok {
			return false
		}
		attempts = append(attempts, normalized)
		return true
	}

	for _, event := range events {
		if invalid {
			break
		}
		switch event.Type {
		case session.EventTurnStart:
			var data session.TurnStartData
			if err := json.Unmarshal(event.Data, &data); err != nil {
				invalid = true
				break
			}
			if sawStart || state.kind != attemptIdle {
				invalid = true
				break
			}
			sawStart = true
			turn = data.Turn
			continue
		}

		if !sawStart {
			invalid = true
			break
		}

		switch event.Type {
		case session.EventTurnEnd:
			var data session.TurnEndData
			if err := json.Unmarshal(event.Data, &data); err != nil {
				invalid = true
				break
			}
			if data.Turn != turn || state.kind != attemptIdle || sawEnd {
				invalid = true
				break
			}
			sawEnd = true

		case session.EventStepStart:
			var data session.StepStartData
			if err := json.Unmarshal(event.Data, &data); err != nil {
				invalid = true
				break
			}
			if data.Turn != turn || state.kind != attemptIdle {
				invalid = true
				break
			}
			state = attemptState{kind: attemptOpen, turn: turn, step: data.Step}

		case EventLlmRetryStarted:
			var data struct {
				Turn int64 `json:"turn"`
				Step int64 `json:"step"`
			}
			if err := json.Unmarshal(event.Data, &data); err != nil {
				invalid = true
				break
			}
			if data.Turn != turn || state.kind != attemptSettled || state.settledBy != "retry" ||
				state.turn != data.Turn || state.step != data.Step {
				invalid = true
				break
			}
			// The retried attempt replays the same step without a fresh
			// step/start, so retry start is the reopen edge.
			state = attemptState{kind: attemptOpen, turn: turn, step: data.Step}

		case EventLlmRetry:
			var data struct {
				Turn int64 `json:"turn"`
				Step int64 `json:"step"`
			}
			if err := json.Unmarshal(event.Data, &data); err != nil {
				invalid = true
				break
			}
			if data.Turn != turn || state.kind == attemptIdle ||
				state.turn != data.Turn || state.step != data.Step {
				invalid = true
				break
			}
			if state.kind == attemptSettled || (state.kind == attemptOpen && !closeOpen(nil)) {
				invalid = true
				break
			}
			state = attemptState{kind: attemptSettled, settledBy: "retry", turn: turn, step: data.Step}

		case session.EventAssistantChunk:
			var data struct {
				Turn  int64           `json:"turn"`
				Step  int64           `json:"step"`
				Chunk llm.StreamChunk `json:"chunk"`
			}
			if err := json.Unmarshal(event.Data, &data); err != nil {
				invalid = true
				break
			}
			if data.Turn != turn || state.kind != attemptOpen ||
				state.turn != data.Turn || state.step != data.Step {
				invalid = true
				break
			}
			if data.Chunk.Type == llm.ChunkUsage {
				if data.Chunk.Usage != nil {
					state.sample = data.Chunk.Usage
				}
				break
			}
			if data.Chunk.Type == llm.ChunkFinish && data.Chunk.Reason != nil &&
				(data.Chunk.Reason.Kind == llm.FinishError || data.Chunk.Reason.Kind == llm.FinishAborted) {
				if !closeOpen(nil) {
					invalid = true
					break
				}
				state = attemptState{kind: attemptFinishClosed, turn: turn, step: data.Step}
			}

		case session.EventAssistantMsg:
			var data session.AssistantMessageData
			if err := json.Unmarshal(event.Data, &data); err != nil {
				invalid = true
				break
			}
			if data.Turn != turn || state.kind != attemptOpen ||
				state.turn != data.Turn || state.step != data.Step {
				invalid = true
				break
			}
			if data.Usage != nil {
				state.sample = data.Usage
			}
			if !closeOpen(messageRoute(data.Message)) {
				invalid = true
				break
			}
			state = attemptState{kind: attemptSettled, settledBy: "message", turn: turn, step: data.Step}

		case session.EventStepEnd:
			var data session.StepEndData
			if err := json.Unmarshal(event.Data, &data); err != nil {
				invalid = true
				break
			}
			if data.Turn != turn || state.kind == attemptIdle ||
				state.turn != data.Turn || state.step != data.Step {
				invalid = true
				break
			}
			if state.kind == attemptOpen && !closeOpen(nil) {
				invalid = true
				break
			}
			state = attemptState{kind: attemptIdle}
		}
	}

	if invalid || !sawEnd || state.kind != attemptIdle {
		return nil
	}
	usage, ok := aggregateAttempts(attempts)
	if !ok {
		return nil
	}
	return usage
}

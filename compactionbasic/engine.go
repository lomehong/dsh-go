package compactionbasic

import (
	"context"
	"fmt"
	"sync"

	"dshgo/agent"
	"dshgo/compaction"
	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/tokenmeter"
)

// Compaction triggers.
const (
	// TriggerPressure is the normal step-boundary pressure check.
	TriggerPressure = "pressure"
	// TriggerContextOverflow is provider-confirmed context-overflow recovery.
	TriggerContextOverflow = "context-overflow"
)

// Pruner is the optional model-free tool-result prune pass
// (dsh-compaction-tool-result-pruner). Compaction remains independently
// composable without it.
type Pruner interface {
	PruneSession(sess *session.Session)
}

// SessionFlusher is the durability checkpoint seam the manual transaction
// flushes through (the sessions registry's flush).
type SessionFlusher interface {
	FlushSession(sess *session.Session) error
}

// MaintenanceAgent is the idle-agent face CompactNow reserves a turn through.
// *agentloop.ReactLoopAgent satisfies it.
type MaintenanceAgent interface {
	AgentView
	RunMaintenance(task func(signal context.Context) error) error
}

// ModelInfoSource resolves the routed model's adapter-owned context capacity.
type ModelInfoSource interface {
	ResolveModelInfo(provider string, model string) (llm.LlmResolvedModelInfo, error)
}

// Engine is the dependency-light replay-aware compaction backend. Summarize
// is the sole customization hook; the replay and durable mutation strategy
// stays fixed so every pricing decision uses the singleton token meter.
type Engine struct {
	// Config is the resolved and validated compaction configuration.
	Config ResolvedConfig

	llm       *llm.Runtime
	meter     *tokenmeter.Meter
	balance   *compaction.ToolPairingBalance
	logger    cordis.Logger
	modelInfo ModelInfoSource
	pruner    Pruner
	flusher   SessionFlusher

	mu                          sync.Mutex
	warnedPressureConfigTargets map[string]bool
	overflowRetries             map[*agent.Agent]int64
	overflowAgents              map[*session.Session]*agent.Agent
}

// EngineConfig wires the engine's seams. ModelInfo is required for the
// pressure trigger and optional otherwise; Pruner rides inside LlmRuntime
// composition and stays a separate seam.
type EngineConfig struct {
	// LLM is the llm runtime summarization calls ride.
	LLM *llm.Runtime
	// Meter is the singleton replay-aware token meter.
	Meter *tokenmeter.Meter
	// Logger receives compaction result and failure logs; nil discards.
	Logger cordis.Logger
	// ModelInfo resolves routed-model capacity for the pressure trigger.
	ModelInfo ModelInfoSource
	// Pruner is the optional model-free prune pass.
	Pruner Pruner
	// Flusher is the optional durability checkpoint for manual compaction.
	Flusher SessionFlusher
}

// NewEngine resolves and validates the configuration (fail loud at load)
// and builds the engine.
func NewEngine(config BasicConfig, engineConfig EngineConfig) (*Engine, error) {
	resolved, err := ResolveConfig(config)
	if err != nil {
		return nil, err
	}
	engine := &Engine{
		Config:                      resolved,
		llm:                         engineConfig.LLM,
		meter:                       engineConfig.Meter,
		balance:                     compaction.NewToolPairingBalance(),
		logger:                      engineConfig.Logger,
		modelInfo:                   engineConfig.ModelInfo,
		pruner:                      engineConfig.Pruner,
		flusher:                     engineConfig.Flusher,
		warnedPressureConfigTargets: map[string]bool{},
		overflowRetries:             map[*agent.Agent]int64{},
		overflowAgents:              map[*session.Session]*agent.Agent{},
	}
	return engine, nil
}

// Dependencies binds the effective token meter and the dynamically
// dispatched Summarize hook.
func (e *Engine) Dependencies() RegionDependencies {
	return RegionDependencies{
		Meter:     e.meter,
		Balance:   e.balance,
		Summarize: e.Summarize,
	}
}

// routedTarget resolves the exact provider/model durably routed for the
// latest request.
func routedTarget(sess *session.Session) *Target {
	header := sess.RequestHeader()
	if header == nil || len(header.Config.Provider) == 0 || len(header.Config.Model) == 0 {
		return nil
	}
	return &Target{Provider: header.Config.Provider, Model: header.Config.Model}
}

// conversationTarget resolves the conversation target used to select an
// optional policy override.
func conversationTarget(view AgentView) *Target {
	routed := routedTarget(view.Session())
	if routed != nil {
		return routed
	}
	if len(view.OptionsProvider()) == 0 || len(view.OptionsModel()) == 0 {
		return nil
	}
	return &Target{Provider: view.OptionsProvider(), Model: view.OptionsModel()}
}

// Summarize the replayed conversation region through a direct one-shot
// stream call whose prefix reuses the conversation's own system prompt,
// tools, and messages so the provider's KV cache is not invalidated.
// Override point: compositions replace the engine's summarize hook for a
// template or remote summarizer.
func (e *Engine) Summarize(input SummarizationInput, owner AgentView, signal context.Context) (SummaryResult, error) {
	target := conversationTarget(owner)
	config := SummaryConfig{
		SummarizationProvider: e.Config.summarizationProvider,
		SummarizationModel:    e.Config.summarizationModel,
		MaxTokens:             e.Config.maxTokens,
	}
	if target != nil {
		policy := ResolveTargetPolicy(e.Config, *target)
		config = SummaryConfig{
			SummarizationProvider: policy.summarizationProvider,
			SummarizationModel:    policy.summarizationModel,
			MaxTokens:             policy.maxTokens,
		}
	}
	return SummarizeWithLlm(e.llm, config, input, owner, signal)
}

// CompactIfNeeded compacts for replayed step-boundary pressure or one
// provider-confirmed context overflow. Both triggers price the latest
// durable routed request envelope; overflow bypasses the normal threshold
// and retained-tail policy so it can force one useful balanced reduction.
// A nil result means no summary ran.
func (e *Engine) CompactIfNeeded(view AgentView, trigger string, signal context.Context) (*compaction.Result, error) {
	target := routedTarget(view.Session())
	if target == nil {
		return nil, nil
	}
	policy := ResolveTargetPolicy(e.Config, *target)
	measurement, err := e.meter.Measure(view.Session(), nil)
	if err != nil {
		return nil, err
	}

	// Pruning is optional so compaction-basic remains independently
	// composable. Overflow always qualifies; pressure first resolves the
	// routed model's capacity and checks its target-specific threshold.
	var pruner Pruner
	if e.pruner != nil {
		pruner = e.pruner
	}

	if trigger == TriggerContextOverflow {
		if pruner != nil {
			pruner.PruneSession(view.Session())
			measurement, err = e.meter.Measure(view.Session(), nil)
			if err != nil {
				return nil, err
			}
		}
		start, end, ok, err := SelectCompactableRange(e.Dependencies(), view.Session(), measurement, 0)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, nil
		}
		result, err := e.CompactRegion(start, end, view, signal)
		if err != nil {
			return nil, err
		}
		return &result, nil
	}

	info, err := e.modelInfo.ResolveModelInfo(target.Provider, target.Model)
	if err != nil {
		return nil, err
	}
	if err := AssertNoActiveCompaction(view.Session(), "automatic pressure compaction"); err != nil {
		return nil, err
	}
	targetKey := target.Provider + "/" + target.Model
	if info.Context == nil {
		return nil, &TargetPressureConfigError{
			TargetKey: targetKey,
			Message: fmt.Sprintf(
				"compaction-basic: no context capacity for %s; configure contextWindow on that adapter model", targetKey),
		}
	}
	spec, err := ResolveCompactSpec(policy, info.Context.ContextWindow)
	if err != nil {
		return nil, err
	}
	if measurement.TotalTokens < spec.ThresholdTokens {
		return nil, nil
	}

	// Once pressure qualifies, land the model-free pass before choosing a
	// summary range, then remeasure through the singleton replay fold.
	if pruner != nil {
		pruner.PruneSession(view.Session())
		measurement, err = e.meter.Measure(view.Session(), nil)
		if err != nil {
			return nil, err
		}
	}
	if measurement.TotalTokens < spec.ThresholdTokens {
		return nil, nil
	}

	var result *compaction.Result
	for attempt := int64(0); attempt <= spec.compactionRetries; attempt++ {
		start, end, ok, err := SelectCompactableRange(e.Dependencies(), view.Session(), measurement, spec.RetainTokens)
		if err != nil {
			return nil, err
		}
		if !ok {
			if result == nil {
				return nil, nil
			}
			break
		}
		compacted, err := e.CompactRegion(start, end, view, signal)
		if err != nil {
			return nil, err
		}
		result = &compacted
		measurement, err = e.meter.Measure(view.Session(), nil)
		if err != nil {
			return nil, err
		}
		if measurement.TotalTokens < spec.ThresholdTokens {
			return result, nil
		}
	}

	return nil, fmt.Errorf(
		"compaction still above threshold after %d compaction attempts (%d estimated tokens >= threshold %d)",
		spec.compactionRetries+1, measurement.TotalTokens, spec.ThresholdTokens)
}

// CompactRegion compacts one inclusive positional range from the
// agent-owned surface using the effective token meter for all retention and
// shrink pricing.
func (e *Engine) CompactRegion(start int64, end int64, view AgentView, signal context.Context) (compaction.Result, error) {
	return CompactSurfaceRegion(
		e.Dependencies(),
		view.Session(),
		start,
		end,
		view,
		TransactionOptions{OwnerCurrentTurn: true, Stability: StabilityWholeSurface},
		signal,
	)
}

// CompactNow forces one useful idle-session compaction below the pressure
// threshold, and resolves only after its standalone marker pair is durably
// checkpointed. The admission failure of a non-idle agent classifies as
// busy; a task cancellation through the maintenance signal classifies as
// cancelled (the source's synchronous catch only wraps runMaintenance's own
// admission error, not the async task result).
func (e *Engine) CompactNow(owner MaintenanceAgent, signal context.Context, sourceCommandID compaction.CommandID) (*compaction.Result, error) {
	if err := signalErr(signal); err != nil {
		return nil, err
	}
	var result *compaction.Result
	taskRan := false
	runErr := owner.RunMaintenance(func(agentSignal context.Context) error {
		taskRan = true
		// AbortSignal.any: the operation aborts when either side cancels.
		operationSignal, cancel := context.WithCancelCause(context.Background())
		defer cancel(nil)
		stop := linkCancellation(operationSignal, cancel, agentSignal, signal)
		defer stop()
		if err := signalErr(operationSignal); err != nil {
			if agentSignal.Err() != nil && context.Cause(operationSignal) == context.Cause(agentSignal) {
				return &ManualCompactionError{
					Kind:    ManualCancelled,
					Message: "manual compaction was cancelled",
					Cause:   err,
				}
			}
			// throwIfAborted rethrows the raw abort reason.
			return err
		}
		measurement, err := e.meter.Measure(owner.Session(), nil)
		if err != nil {
			return err
		}
		start, end, ok, err := SelectCompactableRange(e.Dependencies(), owner.Session(), measurement, 0)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		var flush func() error
		if e.flusher != nil {
			flush = func() error { return e.flusher.FlushSession(owner.Session()) }
		}
		compacted, err := CompactSurfaceRegion(
			e.Dependencies(),
			owner.Session(),
			start,
			end,
			owner,
			TransactionOptions{
				Stability:       StabilitySelectedSpan,
				SourceCommandID: sourceCommandID,
				Flush:           flush,
			},
			operationSignal,
		)
		if err != nil {
			return err
		}
		result = &compacted
		return nil
	})
	if runErr != nil {
		if !taskRan {
			// RunMaintenance refused admission: the agent is not idle.
			return nil, &ManualCompactionError{
				Kind:    ManualBusy,
				Message: "manual compaction requires an idle agent with no waking queued work",
				Cause:   runErr,
			}
		}
		return nil, runErr
	}
	return result, nil
}

// linkCancellation ties a derived operation context to both parent signals
// (AbortSignal.any) and returns the stop func releasing the watchers.
func linkCancellation(operationSignal context.Context, cancel context.CancelCauseFunc, parents ...context.Context) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	for _, parent := range parents {
		wg.Add(1)
		go func(parent context.Context) {
			defer wg.Done()
			select {
			case <-parent.Done():
				select {
				case <-operationSignal.Done():
				case <-done:
				}
				cancel(context.Cause(parent))
			case <-done:
			}
		}(parent)
	}
	return func() {
		close(done)
		wg.Wait()
	}
}

// RegisterAutomaticCompaction registers the automatic between-step pressure
// and model-request overflow recovery listeners on the registry's subject
// bus. CompactIfNeeded stays dynamically dispatched (through the engine
// method) so engine hooks are honored at event time. Go adaptation: the
// optional tool-result pruner rides the engine config; the session/event
// feed reaches ObserveSessionEvent from the composition's session stream.
func (e *Engine) RegisterAutomaticCompaction(bus *agent.SubjectEventBus, listenerScope agent.ScopeKey) (dispose func()) {
	disposers := make([]func(), 0, 3)

	disposers = append(disposers, bus.PreStep().On(listenerScope, func(preStep agent.PreStepPayload, next func(agent.PreStepPayload) agent.PreStepDecision) agent.PreStepDecision {
		if signalErr(preStep.Signal) == nil {
			result, err := e.CompactIfNeeded(ViewAgent(preStep.Agent), TriggerPressure, preStep.Signal)
			if err != nil {
				var pressureErr *TargetPressureConfigError
				if asTargetPressureConfigError(err, &pressureErr) {
					e.mu.Lock()
					alreadyWarned := e.warnedPressureConfigTargets[pressureErr.TargetKey]
					e.warnedPressureConfigTargets[pressureErr.TargetKey] = true
					e.mu.Unlock()
					if alreadyWarned {
						return next(preStep)
					}
				}
				e.logWarn(fmt.Sprintf("step compaction failed: %s; continuing the turn", llm.ErrorChain(err)))
			} else if result != nil {
				e.logResult(*result, "step pressure")
			}
		}
		return next(preStep)
	}))

	disposers = append(disposers, bus.OnEmit(agent.EventAgentStatus, listenerScope, func(payload any) error {
		status, ok := payload.(agent.AgentStatusPayload)
		if !ok || status.Status != agent.AgentIdle {
			return nil
		}
		e.mu.Lock()
		delete(e.overflowRetries, status.Agent)
		e.mu.Unlock()
		return nil
	}))

	disposers = append(disposers, bus.OnWaterfall(agent.EventRequestError, listenerScope, func(payload any, next func(any) any) any {
		requestError, ok := payload.(agent.RequestErrorPayload)
		if !ok {
			return next(payload)
		}
		if requestError.Failure.Code != llm.ContextWindowExceededCode || signalErr(requestError.Signal) != nil {
			return next(payload)
		}
		view := ViewAgent(requestError.Agent)
		e.mu.Lock()
		e.overflowAgents[requestError.Agent.Session] = requestError.Agent
		e.mu.Unlock()
		target := routedTarget(view.Session())
		if target == nil {
			return next(payload)
		}
		policy := ResolveTargetPolicy(e.Config, *target)
		e.mu.Lock()
		retries := e.overflowRetries[requestError.Agent]
		e.mu.Unlock()
		if retries >= policy.maxOverflowRetries {
			return next(payload)
		}

		generation := view.Session().Surface().ReplaceGeneration()
		result, recoveryErr := e.CompactIfNeeded(view, TriggerContextOverflow, requestError.Signal)
		if recoveryErr != nil {
			// A model-free prune can land before later summary work fails.
			// That durable reduction is sufficient retry proof; do not
			// discard it just because the second phase threw. Cancellation
			// still wins.
			if signalErr(requestError.Signal) == nil && view.Session().Surface().ReplaceGeneration() > generation {
				e.logWarn(fmt.Sprintf(
					"context-overflow compaction failed after durable surface progress: %s; retrying from the replacement surface",
					llm.ErrorChain(recoveryErr)))
				e.mu.Lock()
				e.overflowRetries[requestError.Agent] = retries + 1
				e.mu.Unlock()
				return agent.RequestErrorAction{Retry: true}
			}
			aborted := signalErr(requestError.Signal) != nil
			detail := "preserving the original request error"
			if aborted {
				detail = "cancellation prevents retry"
			}
			e.logWarn(fmt.Sprintf("context-overflow compaction failed: %s; %s", llm.ErrorChain(recoveryErr), detail))
			return next(payload)
		}
		if signalErr(requestError.Signal) != nil || view.Session().Surface().ReplaceGeneration() <= generation {
			return next(payload)
		}
		if result != nil {
			e.logResult(*result, "context overflow recovery")
		}
		e.mu.Lock()
		e.overflowRetries[requestError.Agent] = retries + 1
		e.mu.Unlock()
		return agent.RequestErrorAction{Retry: true}
	}))

	return func() {
		for _, dispose := range disposers {
			dispose()
		}
	}
}

// ObserveSessionEvent resets an agent's overflow-recovery sequence when a
// successful response starts even though tool calls continue the same turn
// into another request. The composition feeds session events here.
func (e *Engine) ObserveSessionEvent(sess *session.Session, event session.Event) {
	if event.Type != session.EventAssistantMsg {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	owner, ok := e.overflowAgents[sess]
	if ok {
		delete(e.overflowRetries, owner)
	}
}

// logResult logs one successful compaction in the source's info line.
func (e *Engine) logResult(result compaction.Result, trigger string) {
	if e.logger == nil {
		return
	}
	e.logger.Info(fmt.Sprintf(
		"compaction (%s): shadowed %d surface nodes (seqs %d-%d, ~%d tokens)",
		trigger, len(result.ShadowedSeqs), result.ShadowedRange.Start, result.ShadowedRange.End, result.ShadowedTokenCount))
}

// logWarn logs one contained failure.
func (e *Engine) logWarn(message string) {
	if e.logger == nil {
		return
	}
	e.logger.Warn(message)
}

// asTargetPressureConfigError is the errors.As helper for the typed
// pressure-config failure.
func asTargetPressureConfigError(err error, target **TargetPressureConfigError) bool {
	if pressureErr, ok := err.(*TargetPressureConfigError); ok {
		*target = pressureErr
		return true
	}
	return false
}

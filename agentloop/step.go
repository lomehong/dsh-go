package agentloop

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"dshgo/agent"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/systemprompt"
)

// One model step: request composition, streamed assembly, request-error
// recovery, and tool dispatch. Port of ReactLoopAgent.step and buildRequest in
// packages/core/agent-loop/src/agent.ts. Go adaptations: PrepareCall carries no
// signal (the Go runtime resolves synchronously); markAgentLoopRequest is a
// dev-mode marker with no value-type counterpart.

// assistantChunkData is the historical assistant/chunk payload; format v2
// writers no longer append it (chunks fold into attempt settlements).
type assistantChunkData struct {
	Turn  int64           `json:"turn"`
	Step  int64           `json:"step"`
	Chunk llm.StreamChunk `json:"chunk"`
}

// stepEndReason is the extract of turn endings a single step can produce.
type stepEndReason = session.TurnEndReason

// nextAssistantStreamRevision allocates the next frame revision.
func (d *ReactLoopAgent) nextAssistantStreamRevision() int64 {
	d.assistantStreamRevision++
	return d.assistantStreamRevision
}

// preparedStep is the loop-internal admitted or rejected pre-step result with
// its assembly attached.
type preparedStep struct {
	reject              bool
	messages            []llm.Message
	startsRequestSeries bool
	assembly            *systemprompt.PromptAssembly
}

// step drives one model request and its tool calls. Returns the step's turn
// ending; nil means the turn continues at the next step.
//
// Format-v2 durable discipline: top-level assistant/chunk events are gone.
// Each model attempt settles once — assistant/message (with the embedded
// compact stream) for a successful or cancelled-with-visible-prefix
// response, assistant/attempt for failed/retried/stream-errored attempts —
// and the committed settlement is named by the terminal live frame.
func (d *ReactLoopAgent) step(signal context.Context, turn, step int64, assembly *systemprompt.PromptAssembly, startsRequestSeries bool) (stepEndReason, error) {
	d.mu.Lock()
	phase := d.phase
	d.mu.Unlock()
	if phase.kind != phaseRunning {
		return stepEndReason{}, fmt.Errorf("agent %q: step outside running phase", d.ID)
	}
	if err := signal.Err(); err != nil {
		return stepEndReason{}, err
	}
	system, err := systemprompt.RenderPrompt(assembly)
	if err != nil {
		return stepEndReason{}, err
	}

	for {
		surfaceGeneration := d.Session.Surface().ReplaceGeneration()
		request, preparedCall, err := d.buildRequest(signal, turn, step, assembly.Tools, system, d.Session.DeriveMessages(), startsRequestSeries, surfaceGeneration)
		if err != nil {
			return stepEndReason{}, err
		}
		startsRequestSeries = false
		d.assistantAttemptCounter++
		live := newAssistantStreamAttempt(d.Session.ID(), d.assistantAttemptCounter, d.nextAssistantStreamRevision, turn, step, func(frame agent.AssistantStreamFrame) {
			d.Events().AssistantStream().Publish(d.Scope, frame)
		})
		started := false
		stream := d.loop.LLM.Stream(request)
		if preparedCall != nil {
			stream = preparedCall.Stream(request)
		}
		if err := signal.Err(); err != nil {
			return stepEndReason{}, err
		}
		live.Start(d.nextAssistantStreamRevision())
		started = true
		streamErr := error(nil)
		for chunk := range stream {
			if err := signal.Err(); err != nil {
				streamErr = err
				break
			}
			chunkJSON, marshalErr := json.Marshal(chunk)
			if marshalErr != nil {
				streamErr = marshalErr
				break
			}
			live.Push(llm.TimedStreamChunk{Time: time.Now().UnixMilli(), Chunk: chunk}, d.nextAssistantStreamRevision(), chunkJSON)
		}
		if streamErr == nil {
			streamErr = signal.Err()
		}
		if streamErr != nil {
			if !started {
				return stepEndReason{}, streamErr
			}
			settlementErr := d.settleInterruptedOrAbandoned(signal, live, request)
			if settlementErr != nil {
				return stepEndReason{}, fmt.Errorf("assistant stream failed and its durable settlement was rejected: %v; settlement error: %w", streamErr, settlementErr)
			}
			return stepEndReason{}, streamErr
		}

		finish := live.Finish()
		if finish.Kind == llm.FinishError || finish.Kind == llm.FinishAborted {
			failure := llm.LlmFailure{}
			if finish.Failure != nil {
				failure = *finish.Failure
			}
			if _, settleErr := d.settleAttempt(live); settleErr != nil {
				return stepEndReason{}, settleErr
			}
			action := d.Events().RequestError().Dispatch(d.Scope, agent.RequestErrorPayload{
				Agent:       d.Agent,
				Turn:        turn,
				Step:        step,
				Provider:    request.Provider,
				Failure:     failure,
				RetryPolicy: preparedRetryPolicy(preparedCall),
				Signal:      signal,
			}, func(agent.RequestErrorPayload) agent.RequestErrorAction {
				return agent.RequestErrorAction{}
			})
			if err := signal.Err(); err != nil {
				return stepEndReason{}, err
			}
			if !action.Retry {
				return stepEndReason{}, &llm.LlmError{Harness: llm.NewError(failure.Code, failure.Message, nil), Failure: failure}
			}
			continue
		}

		message := llm.NewAssistantMessage(live.Blocks(), request.Provider, request.Model, marshalReplayState(live.ReplayState()))
		if _, err := d.settleMessage(live, message, false); err != nil {
			return stepEndReason{}, err
		}
		if finish.Kind == llm.FinishMaxTokens {
			return stepEndReason{Kind: session.TurnEndMaxTokens}, nil
		}

		var toolCalls []llm.ContentBlock
		for _, block := range message.Content {
			if block.Type == llm.BlockToolCall {
				toolCalls = append(toolCalls, block)
			}
		}
		if len(toolCalls) == 0 {
			return stepEndReason{Kind: session.TurnEndCompleted}, nil
		}
		scheduler := &toolScheduler{tools: d.loop.Tools, session: d.Session, maxParallel: d.loop.maxParallelToolCalls}
		// The initiator boundary rides the signal context (bound by the
		// driver's kick loop).
		concluded, err := executeToolCalls(scheduler, signal, turn, step, toolCalls, signal, func(context llm.Message) {
			if _, err := d.Inbox.Splice(agent.InboxNextStep, int64(len(d.Inbox.NextStep())), 0, []llm.Message{context}); err != nil {
				panic(fmt.Sprintf("accept tool context: %v", err))
			}
		})
		if err != nil {
			return stepEndReason{}, err
		}
		if concluded {
			return stepEndReason{Kind: session.TurnEndCompleted}, nil
		}
	}
}

// appendAssistantMessage commits one assistant/message settlement with its
// embedded stream (surface append; chunk provenance is obsolete under v2).
func (d *ReactLoopAgent) appendAssistantMessage(live *AssistantStreamAttempt, message llm.Message, interrupted bool) (session.Event, error) {
	stream, err := live.StreamJSON()
	if err != nil {
		return session.Event{}, err
	}
	data := session.AssistantMessageData{
		Turn: live.Turn, Step: live.Step, Message: message,
		Interrupted: interrupted, Stream: stream,
	}
	if usage := live.Usage(); usage != nil {
		data.Usage = usage
	}
	return d.Session.Append(session.EventAssistantMsg, data, &session.SurfaceIntent{
		SurfaceOp: session.SurfaceOp{Kind: session.SurfaceAppend},
	})
}

// settleMessage commits the message settlement then publishes the committed
// end frame.
func (d *ReactLoopAgent) settleMessage(live *AssistantStreamAttempt, message llm.Message, interrupted bool) (session.Event, error) {
	return live.Settle(session.EventAssistantMsg, d.nextAssistantStreamRevision(), func() (session.Event, error) {
		return d.appendAssistantMessage(live, message, interrupted)
	})
}

// settleAttempt commits the log-only attempt settlement then publishes the
// committed end frame.
func (d *ReactLoopAgent) settleAttempt(live *AssistantStreamAttempt) (session.Event, error) {
	return live.Settle(session.EventAssistantAttempt, d.nextAssistantStreamRevision(), func() (session.Event, error) {
		stream, err := live.StreamJSON()
		if err != nil {
			return session.Event{}, err
		}
		return d.Session.Append(session.EventAssistantAttempt, session.AssistantAttemptData{
			Turn: live.Turn, Step: live.Step, Stream: stream,
		}, nil)
	})
}

// settleInterruptedOrAbandoned finalizes a stream that died mid-flight:
// a cancellation with visible content settles an interrupted message,
// everything else settles a log-only attempt.
func (d *ReactLoopAgent) settleInterruptedOrAbandoned(signal context.Context, live *AssistantStreamAttempt, request llm.GenerateOptions) error {
	if signal.Err() != nil {
		content := live.InterruptedBlocks()
		if len(content) > 0 {
			interrupted := llm.NewAssistantMessage(content, request.Provider, request.Model, marshalReplayState(live.ReplayState()))
			_, err := d.settleMessage(live, interrupted, true)
			return err
		}
	}
	_, err := d.settleAttempt(live)
	return err
}

// marshalReplayState renders the attempt's replay envelope for the message
// source (nil stays absent).
func marshalReplayState(replay *llm.ReplayEnvelope) json.RawMessage {
	if replay == nil {
		return nil
	}
	encoded, err := json.Marshal(replay)
	if err != nil {
		return nil
	}
	return encoded
}

func preparedRetryPolicy(prepared *llm.PreparedCall) *llm.ResolvedRetryPolicy {
	if prepared == nil {
		return nil
	}
	return prepared.RetryPolicy
}

// requestProposal removes adapter-derived values before plugins propose the
// next request config.
func requestProposal(header *session.EpochHeader) llm.LlmCallConfig {
	proposal := header.Config
	if header.AdapterDefaults == nil {
		return proposal
	}
	if header.AdapterDefaults.ReasoningEffort {
		proposal.ReasoningEffort = ""
	}
	if header.AdapterDefaults.MaxTokens {
		proposal.MaxTokens = nil
	}
	return proposal
}

// buildRequest composes one request and binds it to the adapter registration
// that resolved its exact-model defaults.
func (d *ReactLoopAgent) buildRequest(
	signal context.Context,
	turn, step int64,
	tools []llm.ToolSchema,
	system string,
	boundaryMessages []llm.Message,
	startsRequestSeries bool,
	surfaceGeneration int64,
) (llm.GenerateOptions, *llm.PreparedCall, error) {
	// A loop instance starts from its declared route, restoring only an
	// explicit effort owned by that exact model. Later steps re-resolve
	// marked defaults.
	persistedHeader := d.Session.RequestHeader()
	route := llm.LlmCallConfig{Provider: d.Options.Provider, Model: d.Options.Model}
	var persistedReasoningEffort llm.ReasoningEffortID
	if persistedHeader != nil &&
		persistedHeader.Config.Provider == route.Provider &&
		persistedHeader.Config.Model == route.Model &&
		(persistedHeader.AdapterDefaults == nil || !persistedHeader.AdapterDefaults.ReasoningEffort) {
		persistedReasoningEffort = persistedHeader.Config.ReasoningEffort
	}
	reasoningEffort := d.Options.ReasoningEffort
	if reasoningEffort == "" {
		reasoningEffort = persistedReasoningEffort
	}
	var seedConfig llm.LlmCallConfig
	if d.requestHeaderLogged {
		seedConfig = requestProposal(persistedHeader)
	} else {
		seedConfig = llm.LlmCallConfig{Provider: route.Provider, Model: route.Model}
		if reasoningEffort != "" {
			seedConfig.ReasoningEffort = reasoningEffort
		}
		if d.Options.MaxTokens != nil {
			seedConfig.MaxTokens = d.Options.MaxTokens
		}
	}
	payload := agent.RequestPayload{Agent: d.Agent, Turn: turn, Step: step, Signal: signal}
	proposed := *d.Events().Request().Dispatch(d.Scope, payload, func(agent.RequestPayload) *llm.LlmCallConfig {
		return &seedConfig
	})
	if err := signal.Err(); err != nil {
		return llm.GenerateOptions{}, nil, err
	}
	if proposed.Provider == "" || proposed.Model == "" {
		return llm.GenerateOptions{}, nil, fmt.Errorf("agent %q has no provider/model: set AgentOptions.provider and AgentOptions.model or supply both via the agent/request waterfall", d.ID)
	}
	var config llm.LlmCallConfig
	preparedCall, prepareErr := d.loop.LLM.PrepareCall(proposed)
	switch {
	case prepareErr == nil:
		config = preparedCall.Config
	case isNoAdapter(prepareErr):
		// Middleware may serve an unregistered route; terminal dispatch still
		// requires an adapter.
		config = proposed
	default:
		return llm.GenerateOptions{}, nil, prepareErr
	}
	if err := signal.Err(); err != nil {
		return llm.GenerateOptions{}, nil, err
	}

	header := session.CanonicalHeader(session.EpochHeader{
		Config:          config,
		AdapterDefaults: preparedAdapterDefaults(preparedCall),
		System:          system,
		Tools:           tools,
	})
	baseline := d.Session.RequestHeader()
	startsSeries := startsRequestSeries || !d.hasRequestSurfaceGeneration || d.requestSurfaceGeneration != surfaceGeneration
	headerData := session.RequestHeaderData{Header: header}
	switch {
	case !d.requestHeaderLogged:
		if baseline == nil {
			headerData.Reason = session.HeaderReasonInitial
		} else {
			headerData.Reason = session.HeaderReasonResume
		}
		d.requestHeaderLogged = true
	case baseline == nil || !session.HeaderEquals(*baseline, header):
		headerData.Reason = session.HeaderReasonChange
		headerData.StartsSeries = startsSeries
	case startsSeries:
		headerData.Reason = session.HeaderReasonSeries
	default:
		headerData.Reason = ""
	}
	if headerData.Reason != "" {
		if _, err := d.Session.Append(session.EventRequestHeader, headerData, nil); err != nil {
			return llm.GenerateOptions{}, nil, err
		}
	}
	d.hasRequestSurfaceGeneration = true
	d.requestSurfaceGeneration = surfaceGeneration

	requestContext := session.RequestContext{Provider: config.Provider, Model: config.Model}
	// The prepared call owns exact-model context capacity; an unregistered
	// route falls back to resolved model info, and unresolved metadata just
	// omits the window.
	if window := requestContextWindow(d.loop.LLM, preparedCall, config.Provider, config.Model); window != nil {
		requestContext.ContextWindow = window
	}
	previousContext := d.Session.RequestContext()
	if previousContext == nil || previousContext.Provider != requestContext.Provider || previousContext.Model != requestContext.Model || !sameWindow(previousContext.ContextWindow, requestContext.ContextWindow) {
		if _, err := d.Session.Append(session.EventRequestCtx, requestContext, nil); err != nil {
			return llm.GenerateOptions{}, nil, err
		}
	}
	if err := signal.Err(); err != nil {
		return llm.GenerateOptions{}, nil, err
	}

	request := llm.GenerateOptions{
		Provider:    config.Provider,
		Model:       config.Model,
		Temperature: config.Temperature,
		MaxTokens:   config.MaxTokens,
		Stop:        config.Stop,
		Messages:    boundaryMessages,
		System:      header.System,
		Tools:       header.Tools,
		SessionID:   string(d.Session.ID()),
		Context:     signal,
	}
	return request, preparedCall, nil
}

func isNoAdapter(err error) bool {
	llmErr, ok := err.(*llm.LlmError)
	return ok && llmErr.Code() == llm.CodeNoAdapter
}

// requestContextWindow resolves the display context capacity from the owning
// adapter's model info; unresolved metadata yields nil.
func requestContextWindow(runtime *llm.Runtime, prepared *llm.PreparedCall, provider, model string) *int64 {
	_ = prepared
	info, err := runtime.ResolveModelInfo(provider, model)
	if err != nil || info.Context == nil {
		return nil
	}
	window := info.Context.ContextWindow
	return &window
}

func preparedAdapterDefaults(prepared *llm.PreparedCall) *llm.LlmCallConfigAdapterDefaults {
	if prepared == nil {
		return nil
	}
	return prepared.AdapterDefaults
}

func sameWindow(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// jsonUnmarshal is the package-local decode used by the driver's resume scan.
func jsonUnmarshal(data []byte, target any) error {
	return json.Unmarshal(data, target)
}

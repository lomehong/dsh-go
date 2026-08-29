package agentloop

import (
	"context"
	"encoding/json"
	"fmt"

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

// assistantChunkData is the assistant/chunk payload.
type assistantChunkData struct {
	Turn  int64           `json:"turn"`
	Step  int64           `json:"step"`
	Chunk llm.StreamChunk `json:"chunk"`
}

// stepEndReason is the extract of turn endings a single step can produce.
type stepEndReason = session.TurnEndReason

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
		assembler := llm.NewBlockAssembler()
		var chunkSeqs []int64
		stream := d.loop.LLM.Stream(request)
		if preparedCall != nil {
			stream = preparedCall.Stream(request)
		}
		if err := signal.Err(); err != nil {
			return stepEndReason{}, err
		}
		streamErr := error(nil)
		for chunk := range stream {
			if err := signal.Err(); err != nil {
				streamErr = err
				break
			}
			event, appendErr := d.Session.Append(session.EventAssistantChunk, assistantChunkData{Turn: turn, Step: step, Chunk: chunk}, nil)
			if appendErr != nil {
				streamErr = appendErr
				break
			}
			chunkSeqs = append(chunkSeqs, event.Seq)
			assembler.Push(chunk)
		}
		if streamErr == nil {
			streamErr = signal.Err()
		}
		if streamErr != nil {
			// A cancelled mid-stream finalizes its delivered text/reasoning
			// prefix; undispatched tool calls are absent.
			if signal.Err() != nil {
				content := assembler.InterruptedBlocks()
				if len(content) > 0 {
					interrupted := llm.NewAssistantMessage(content, request.Provider, request.Model, nil)
					data := session.AssistantMessageData{Turn: turn, Step: step, Message: interrupted, Interrupted: true}
					if usage := assembler.Usage(); usage != nil {
						data.Usage = usage
					}
					if _, err := d.Session.Append(session.EventAssistantMsg, data, &session.SurfaceIntent{
						SurfaceOp:         session.SurfaceOp{Kind: session.SurfaceAppend},
						SourceEventSeqs:   chunkSeqs,
						SourceSeqsPresent: true,
					}); err != nil {
						return stepEndReason{}, err
					}
				}
			}
			return stepEndReason{}, streamErr
		}

		finish := assembler.Finish()
		if finish.Kind == llm.FinishError || finish.Kind == llm.FinishAborted {
			failure := llm.LlmFailure{}
			if finish.Failure != nil {
				failure = *finish.Failure
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

		message := assembler.Message(request.Provider, request.Model)
		data := session.AssistantMessageData{Turn: turn, Step: step, Message: message}
		if usage := assembler.Usage(); usage != nil {
			data.Usage = usage
		}
		if chunkSeqs == nil {
			chunkSeqs = []int64{}
		}
		if _, err := d.Session.Append(session.EventAssistantMsg, data, &session.SurfaceIntent{
			SurfaceOp:         session.SurfaceOp{Kind: session.SurfaceAppend},
			SourceEventSeqs:   chunkSeqs,
			SourceSeqsPresent: true,
		}); err != nil {
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

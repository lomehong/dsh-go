// Package sessiontitlellm ports @deepseek-ai/dsh-session-title-llm: the
// shared route, framing, timeout, assembly, and validation policy for
// model-backed session-title providers. The shipped first-prompt plugin is a
// two-line selection over this policy, so the package also carries the
// register helper and the official first-prompt message selector.
package sessiontitlellm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"dshgo/llm"
	"dshgo/session"
	"dshgo/sessionquery"
	"dshgo/sessiontitle"
)

// SessionTitleTimeoutCode is the capability-owned timeout reason code for
// auxiliary title requests.
const SessionTitleTimeoutCode = "SESSION_TITLE_TIMEOUT"

// Config is the required deployment policy for one model-backed title
// plugin. Provider/model are an all-or-nothing pair: both present pins the
// auxiliary route; both absent resolves the route from the request the
// service captured from the session's logged request/header.
type Config struct {
	// TargetWords is the target word count for non-CJK titles.
	TargetWords int
	// TargetCJKCharacters is the target character count for CJK titles.
	TargetCJKCharacters int
	// MaxInputBytes is the maximum UTF-8 bytes in the framed user prompt.
	MaxInputBytes int
	// MaxOutputTokens is the auxiliary generation output-token cap.
	MaxOutputTokens int64
	// TimeoutMs is the end-to-end auxiliary request deadline in
	// milliseconds.
	TimeoutMs int64
	// Provider is the optional pinned auxiliary provider route.
	Provider string
	// Model is the optional pinned auxiliary model id.
	Model string
}

// ResolveConfig validates one policy fail-loud: every numeric field must be
// positive and provider/model must come together or not at all.
func ResolveConfig(config Config) (Config, error) {
	if config.TargetWords <= 0 {
		return Config{}, errors.New("session-title-llm: targetWords must be positive")
	}
	if config.TargetCJKCharacters <= 0 {
		return Config{}, errors.New("session-title-llm: targetCjkCharacters must be positive")
	}
	if config.MaxInputBytes <= 0 {
		return Config{}, errors.New("session-title-llm: maxInputBytes must be positive")
	}
	if config.MaxOutputTokens <= 0 {
		return Config{}, errors.New("session-title-llm: maxOutputTokens must be positive")
	}
	if config.TimeoutMs <= 0 {
		return Config{}, errors.New("session-title-llm: timeoutMs must be positive")
	}
	if (config.Provider == "") != (config.Model == "") {
		return Config{}, errors.New("session-title-llm: provider and model must be configured together")
	}
	return config, nil
}

// SelectFirstPrompt returns the official first-prompt selection: exactly the
// first eligible human message.
func SelectFirstPrompt(messages []sessionquery.SessionTitleUserMessage) ([]sessionquery.SessionTitleUserMessage, error) {
	if len(messages) == 0 {
		return nil, errors.New("first-prompt title provider requires one human message")
	}
	return messages[:1], nil
}

// Register installs one model-backed title provider on the service. The
// returned closer withdraws the registration. Deviation: the official helper
// logs a `session/title-llm-request` event onto the titled session before
// dispatch; the Go port keeps that record (see below) with the same payload
// shape.
func Register(
	service *sessiontitle.Service,
	runtime *llm.Runtime,
	config Config,
	id string,
	automatic string,
	selectMessages func([]sessionquery.SessionTitleUserMessage) ([]sessionquery.SessionTitleUserMessage, error),
) (func(), error) {
	resolved, err := ResolveConfig(config)
	if err != nil {
		return nil, err
	}
	if runtime == nil {
		return nil, errors.New("session-title-llm: the llm runtime is required")
	}
	session.EnsureEventTypes("session/title-llm-request")
	return service.RegisterProvider(sessiontitle.ProviderFunc{
		IDFunc:        func() string { return id },
		AutomaticFunc: func() string { return automatic },
		GenerateFunc: func(request sessiontitle.ProviderRequest) (sessiontitle.ProviderResult, error) {
			selected, err := selectMessages(request.Messages)
			if err != nil {
				return sessiontitle.ProviderResult{}, err
			}
			return generateWithLlm(runtime, resolved, request, selected, id)
		},
	})
}

func systemPrompt(config Config) string {
	return strings.Join([]string{
		"Create a concise title for an AI coding-assistant session from the supplied human messages.",
		"Return only the title on one line, **in plain text of natural language**, with no quotes, prefix, explanation, Markdown, XML, or terminal control codes. No code is allowed.",
		"Use the language of the messages.",
		fmt.Sprintf("Aim for about %d words in non-CJK languages or %d CJK characters.", config.TargetWords, config.TargetCJKCharacters),
	}, "\n")
}

// frameMessages frames exact messages as JSON so user text cannot break
// structural delimiters (the official framing, field-for-field).
func frameMessages(messages []sessionquery.SessionTitleUserMessage) (string, error) {
	encoded, err := json.Marshal(messages)
	if err != nil {
		return "", fmt.Errorf("session-title-llm: frame messages: %w", err)
	}
	return "Generate the session title from this JSON array of human messages:\n" + string(encoded), nil
}

func resolveRoute(config Config, request sessiontitle.ProviderRequest) (sessionquery.SessionTitleModelProvenance, error) {
	if config.Provider != "" && config.Model != "" {
		return sessionquery.SessionTitleModelProvenance{Provider: config.Provider, Model: config.Model}, nil
	}
	if request.Route == nil {
		return sessionquery.SessionTitleModelProvenance{}, errors.New("session-title-llm: no logged request route is available; configure provider and model together")
	}
	return *request.Route, nil
}

func generateWithLlm(
	runtime *llm.Runtime,
	config Config,
	request sessiontitle.ProviderRequest,
	selected []sessionquery.SessionTitleUserMessage,
	titleProvider string,
) (sessiontitle.ProviderResult, error) {
	if err := request.Signal.Err(); err != nil {
		return sessiontitle.ProviderResult{}, err
	}
	if len(selected) == 0 {
		return sessiontitle.ProviderResult{}, errors.New("session-title-llm: at least one source message is required")
	}
	framedInput, err := frameMessages(selected)
	if err != nil {
		return sessiontitle.ProviderResult{}, err
	}
	if len(framedInput) > config.MaxInputBytes {
		return sessiontitle.ProviderResult{}, fmt.Errorf("session-title-llm: input is %d bytes, exceeding maxInputBytes %d", len(framedInput), config.MaxInputBytes)
	}
	route, err := resolveRoute(config, request)
	if err != nil {
		return sessiontitle.ProviderResult{}, err
	}
	system := systemPrompt(config)
	seqs := make([]int64, 0, len(selected))
	for _, message := range selected {
		seqs = append(seqs, message.Seq)
	}
	// Log-only pre-dispatch record of the exact auxiliary request.
	if _, err := request.Session.Append("session/title-llm-request", map[string]any{
		"titleProvider": titleProvider,
		"messageSeqs":   seqs,
		"route":         route,
		"system":        system,
		"maxTokens":     config.MaxOutputTokens,
	}, nil); err != nil {
		return sessiontitle.ProviderResult{}, err
	}
	ctx, cancel := context.WithTimeout(request.Signal, time.Duration(config.TimeoutMs)*time.Millisecond)
	defer cancel()
	options := llm.GenerateOptions{
		Provider: route.Provider,
		Model:    route.Model,
		Messages: []llm.Message{{
			Role:    llm.RoleUser,
			Source:  llm.MessageSource{Kind: llm.SourcePlugin, Plugin: "dsh-session-title-llm"},
			Content: []llm.ContentBlock{{Type: llm.BlockText, Text: framedInput}},
		}},
		System:    system,
		MaxTokens: &config.MaxOutputTokens,
		SessionID: request.Session.ID(),
		Purpose:   llm.PurposeSessionTitle,
		Context:   ctx,
	}
	assembler := llm.NewBlockAssembler()
	for chunk := range runtime.Stream(options) {
		if ctx.Err() != nil {
			return sessiontitle.ProviderResult{}, ctx.Err()
		}
		assembler.Push(chunk)
		if chunk.Type == llm.ChunkFinish && chunk.Reason != nil {
			if err := finishError(*chunk.Reason); err != nil {
				return sessiontitle.ProviderResult{}, err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return sessiontitle.ProviderResult{}, fmt.Errorf("session-title-llm: auxiliary title request timed out (%s)", SessionTitleTimeoutCode)
		}
		return sessiontitle.ProviderResult{}, err
	}
	blocks := assembler.Blocks()
	var texts []string
	for _, block := range blocks {
		if block.Type == llm.BlockToolCall {
			return sessiontitle.ProviderResult{}, errors.New("session-title-llm: title output must contain text only")
		}
		if block.Type == llm.BlockText {
			texts = append(texts, block.Text)
		}
	}
	title := sessionquery.NormalizeSessionTitle(strings.Join(texts, " "), int(^uint(0)>>1))
	if title == "" {
		return sessiontitle.ProviderResult{}, errors.New("session-title-llm: title model produced no text")
	}
	usedRoute := route
	return sessiontitle.ProviderResult{
		Title:       title,
		MessageSeqs: seqs,
		Model:       &usedRoute,
	}, nil
}

// finishError translates terminal finish reasons into an auxiliary-call
// failure.
func finishError(finish llm.FinishReason) error {
	switch finish.Kind {
	case llm.FinishStop:
		return nil
	case llm.FinishError, llm.FinishAborted:
		message := finish.Kind
		if finish.Failure != nil {
			message = finish.Failure.Message
		}
		return fmt.Errorf("session-title-llm: %s", message)
	case llm.FinishMaxTokens:
		return errors.New("session-title-llm: title output reached maxOutputTokens")
	case llm.FinishToolCalls:
		return errors.New("session-title-llm: title model unexpectedly requested a tool")
	default:
		return fmt.Errorf("session-title-llm: unsupported finish reason %q", finish.Kind)
	}
}

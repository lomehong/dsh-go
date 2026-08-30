// Workspace-backed Session creation for one settled webhook rule result.
// Port of packages/webhook/webhook/src/session.ts; the transaction order,
// the refusal wordings, and the rollback policy stay verbatim.
package webhook

import (
	"context"
	"fmt"
	"path/filepath"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/identity"
	"dshgo/interaction/permissionpresets"
	"dshgo/llm"
	"dshgo/preset"
	"dshgo/session"
	"dshgo/sessionquery"
	"dshgo/workspace"
)

// AgentCreator is the creation face of the agent registry this transaction
// programs (ctx.agents.create in the source).
type AgentCreator interface {
	Create(ctx context.Context, options agent.CreateAgentOptions) (agent.AgentHandle, error)
}

// SessionTitler renames the session at creation time (ctx.sessionTitle in
// the source).
type SessionTitler interface {
	Rename(sess *session.Session, title string) (*sessionquery.SessionTitleSnapshot, error)
}

// SessionDeps are the seams the creation transaction reads (the source
// reaches them through the runtime context's services).
type SessionDeps struct {
	// Logger receives rollback-failure warnings; nil logs nothing.
	Logger cordis.Logger
	// DefaultModel is the complete current default selection
	// (agentDefaultModel.currentSelection) used when the request names no
	// explicit model.
	DefaultModel func() agent.ModelSelection
	// PermissionPresets validates and applies the named preset.
	PermissionPresets *permissionpresets.Service
	// Presets resolves and mounts the agent composition.
	Presets *preset.Mounts
	// Workspaces resolves or creates the Web Workspace.
	Workspaces *workspace.Registry
	// Agents creates the root agent.
	Agents AgentCreator
	// Titles renames the session after creation.
	Titles SessionTitler
}

// resolvedRequest is the detached values the creation transaction keeps
// across asynchronous preflight.
type resolvedRequest struct {
	workspacePath    string
	title            string
	prompt           string
	agentPreset      string
	permissionPreset string
	modelSelection   agent.ModelSelection
	agentOptions     agent.AgentOptions
}

// requiredString enforces one non-empty string field (the source's
// requiredString over its untyped rule result).
func requiredString(field string, value string) (string, error) {
	if trimSpace(value) == "" {
		return "", fmt.Errorf("webhook Session request %s must be a non-empty string", field)
	}
	return value, nil
}

// resolveRequest snapshots and validates the same-process rule result
// before crossing awaits.
func resolveRequest(deps SessionDeps, input WebhookSessionRequest) (resolvedRequest, error) {
	workspacePath, err := requiredString("workspacePath", input.WorkspacePath)
	if err != nil {
		return resolvedRequest{}, err
	}
	if !filepath.IsAbs(workspacePath) {
		return resolvedRequest{}, fmt.Errorf("webhook Session request workspacePath must be absolute, got %q", workspacePath)
	}
	title, err := requiredString("title", input.Title)
	if err != nil {
		return resolvedRequest{}, err
	}
	prompt, err := requiredString("prompt", input.Prompt)
	if err != nil {
		return resolvedRequest{}, err
	}
	agentPreset, err := requiredString("agentPreset", input.AgentPreset)
	if err != nil {
		return resolvedRequest{}, err
	}
	permissionPreset, err := requiredString("permissionPreset", input.PermissionPreset)
	if err != nil {
		return resolvedRequest{}, err
	}
	var agentOptions agent.AgentOptions
	var modelSelection agent.ModelSelection
	if input.Model == nil {
		if deps.DefaultModel == nil {
			return resolvedRequest{}, fmt.Errorf("webhook Session request has no model and no default selection is composed")
		}
		selected := deps.DefaultModel()
		agentOptions = agent.AgentOptions{Provider: selected.Provider, Model: selected.Model}
		modelSelection = selected
	} else {
		provider, err := requiredString("provider", input.Model.Provider)
		if err != nil {
			return resolvedRequest{}, err
		}
		modelID, err := requiredString("model", input.Model.Model)
		if err != nil {
			return resolvedRequest{}, err
		}
		agentOptions = agent.AgentOptions{Provider: provider, Model: modelID}
		if input.Model.MaxTokens != nil {
			if *input.Model.MaxTokens <= 0 || *input.Model.MaxTokens > maxSafeInteger {
				return resolvedRequest{}, fmt.Errorf("webhook Session request model.maxTokens must be a positive safe integer")
			}
			tokens := *input.Model.MaxTokens
			agentOptions.MaxTokens = &tokens
		}
		modelSelection = agent.ModelSelection{Provider: provider, Model: modelID}
	}
	return resolvedRequest{
		workspacePath:    workspacePath,
		title:            title,
		prompt:           prompt,
		agentPreset:      agentPreset,
		permissionPreset: permissionPreset,
		modelSelection:   modelSelection,
		agentOptions:     agentOptions,
	}, nil
}

// reportRollbackFailure logs a rollback failure without replacing the
// operation's original failure.
func reportRollbackFailure(logger cordis.Logger, subject string, err error) {
	if logger == nil {
		return
	}
	logger.Warn(fmt.Sprintf("webhook: %s rollback failed: %s", subject, llm.ErrorChain(err)))
}

// installInitialModelSelection applies the creation-time selection until the
// session's first durable request header exists. The listener unwinds with
// the agent's scoped context, exactly like the source's ctx-scoped on().
func installInitialModelSelection(agentCtx *cordis.Context, selection agent.ModelSelection) error {
	target, ok := agent.ContextService.From(agentCtx)
	if !ok {
		return fmt.Errorf("webhook Session setup has no scoped Agent")
	}
	dispose := target.Events().Request().On(target.Scope, func(payload agent.RequestPayload, next func(agent.RequestPayload) *llm.LlmCallConfig) *llm.LlmCallConfig {
		resolved := next(payload)
		built, ok := agent.ContextService.From(agentCtx)
		if !ok {
			// The source throws here; an agentless dispatch on a live
			// agent bus is structurally impossible.
			panic("webhook Session setup has no scoped Agent")
		}
		if resolved == nil {
			return nil
		}
		if built.Session.RequestHeader() != nil ||
			resolved.Provider != selection.Provider ||
			resolved.Model != selection.Model {
			return resolved
		}
		out := *resolved
		out.ReasoningEffort = ""
		if selection.HasReasoningEffort {
			out.ReasoningEffort = selection.ReasoningEffort
		}
		return &out
	})
	return agentCtx.Effect(func() (cordis.Disposer, error) {
		return cordis.Disposer(dispose), nil
	})
}

// CreateWebhookSession creates, attaches, titles, configures, and prompts
// one ordinary root Session. Successful prompt admission ends webhook
// ownership of the operation; the Agent remains lifecycle-owned by the
// registry's context and follows normal Session behavior.
//
// delivery is the exact verified provider delivery used for provenance;
// ruleID is the rule that returned the request; signal is the registration
// lifetime cancellation through publication.
func CreateWebhookSession(deps SessionDeps, delivery VerifiedWebhookDelivery, ruleID WebhookRuleID, request WebhookSessionRequest, signal context.Context) error {
	resolved, err := resolveRequest(deps, request)
	if err != nil {
		return err
	}
	if _, err := deps.PermissionPresets.Resolve(resolved.permissionPreset); err != nil {
		return err
	}
	presetRow, err := deps.Presets.Resolve(resolved.agentPreset)
	if err != nil {
		return err
	}
	if _, err := deps.Presets.StandingKeyFor(presetRow.ID); err != nil {
		return err
	}
	if err := signal.Err(); err != nil {
		return err
	}

	entity, err := deps.Workspaces.Create(signal, resolved.workspacePath, "")
	if err != nil {
		return err
	}
	if err := signal.Err(); err != nil {
		return err
	}
	sessionID := session.SessionID("webhook-" + identity.RandomUUID())
	handle, err := deps.Agents.Create(signal, agent.CreateAgentOptions{
		SessionID:    sessionID,
		Meta:         agent.CreateAgentMeta{CWD: entity.Path(), AgentPreset: presetRow.ID},
		AgentOptions: resolved.agentOptions,
		Setup: func(agentCtx *cordis.Context) (agent.AgentSetupCommit, error) {
			if _, err := deps.Presets.Mount(agentCtx, presetRow.ID); err != nil {
				return agent.AgentSetupCommit{}, err
			}
			if err := installInitialModelSelection(agentCtx, resolved.modelSelection); err != nil {
				return agent.AgentSetupCommit{}, err
			}
			return agent.AgentSetupCommit{}, nil
		},
	})
	if err != nil {
		return err
	}

	attached := false
	if err := func() error {
		if err := signal.Err(); err != nil {
			return err
		}
		if err := entity.AttachSession(sessionID); err != nil {
			return err
		}
		attached = true
		if err := signal.Err(); err != nil {
			return err
		}
		if err := deps.PermissionPresets.Set(handle.Agent.Session, resolved.permissionPreset); err != nil {
			return err
		}
		if _, err := deps.Titles.Rename(handle.Agent.Session, resolved.title); err != nil {
			return err
		}
		handle.Agent.Driver().Followup(llm.NewUserMessage([]llm.ContentBlock{{Type: "text", Text: resolved.prompt}}, llm.MessageSource{
			Kind:          llm.SourceWebhook,
			Provider:      delivery.Kind,
			WebhookSource: delivery.Source,
			DeliveryID:    delivery.DeliveryID,
			RuleID:        ruleID,
			Form:          llm.FormNotice,
			Summary:       llm.BoundContextSummary(fmt.Sprintf("%s webhook handled by %s", delivery.Kind, ruleID)),
		}))
		return nil
	}(); err != nil {
		if attached {
			if detachErr := entity.DetachSession(sessionID); detachErr != nil {
				reportRollbackFailure(deps.Logger, fmt.Sprintf("Workspace detach for Session %q", sessionID), detachErr)
			}
		}
		if disposeErr := handle.Dispose(); disposeErr != nil {
			reportRollbackFailure(deps.Logger, fmt.Sprintf("Agent disposal for Session %q", sessionID), disposeErr)
		}
		return err
	}
	return nil
}

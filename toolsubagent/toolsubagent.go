// Package toolsubagent ports @deepseek-ai/dsh-tool-subagent: model-facing
// delegation through one configured subagent provider. Provider capabilities
// control tool registration and context-sensitive schema wording. Foreground
// calls always dispose the run after collection. Background policy is the
// plugin configuration: one-shot calls own a plain job, while continuable
// calls start a durable child conversation and return its id.
//
// Go adaptations recorded in the README decision log: the provider must be
// present at registration (the catalog composes in-process providers before
// this tool; official tolerates late arrival via registry listeners), and the
// child model-selection face (provider/model/reasoning_effort parameters,
// list_subagent_models, settings sampling) is deferred — child LLM routing
// stays with configured AgentOptions only.
package toolsubagent

import (
	"context"
	"fmt"
	"strings"

	"dshgo/agent"
	"dshgo/jobs"
	"dshgo/llm"
	"dshgo/scope"
	"dshgo/subagent"
	"dshgo/systemprompt"
	"dshgo/tools"
)

// BackgroundMode selects when calls run in the background by default and
// which start path a background request takes.
type BackgroundMode string

// The background policies. OneShot keeps the foreground default and routes a
// background request through the jobs registry; Continuable defaults calls to
// background and starts a durable child conversation instead.
const (
	BackgroundOneShot     BackgroundMode = "one-shot"
	BackgroundContinuable BackgroundMode = "continuable"
)

// ToolFilterConfig is the model-facing child tool filter: the named global
// tools the child keeps (Allow) or loses (Deny). Both empty is rejected at
// load — an empty filter would silently deny every tool.
type ToolFilterConfig struct {
	Allow []string `json:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

// Config is the plugin configuration: which registered provider this tool
// delegates to, plus child defaults.
type Config struct {
	// Provider is the subagent registry name to start runs on (spawn, fork…).
	Provider string `json:"provider"`
	// ToolName is the model-facing tool name; each loaded instance must use
	// a distinct name. Empty resolves to "subagent".
	ToolName string `json:"toolName,omitempty"`
	// EnableRunInBackground exposes run_in_background (default true). A
	// disabled instance omits the parameter and rejects forced background
	// calls.
	EnableRunInBackground bool `json:"enableRunInBackground,omitempty"`
	// EnableRunInBackgroundSet marks an explicit value (including false).
	EnableRunInBackgroundSet bool `json:"-"`
	// BackgroundMode is the background execution policy (default one-shot).
	BackgroundMode BackgroundMode `json:"backgroundMode,omitempty"`
	// AgentOptions applies provider/model/reasoning-effort/token overrides
	// to every child; requires the AgentOptions capability.
	AgentOptions *agent.AgentOptions `json:"agentOptions,omitempty"`
	// Persona is the per-child persona shadowing deployment:persona;
	// requires the Persona capability.
	Persona string `json:"persona,omitempty"`
	// ToolFilter scopes every child's tools; requires the ToolFilter
	// capability.
	ToolFilter *ToolFilterConfig `json:"toolFilter,omitempty"`
	// MaxDepth is the absolute delegation-depth cap for each child;
	// requires the DepthLimit capability. Zero means unset.
	MaxDepth int64 `json:"maxDepth,omitempty"`
	// MaxDepthProviderManaged sends no cap: the recursion budget belongs to
	// the child runtime (out-of-process providers).
	MaxDepthProviderManaged bool `json:"maxDepthProviderManaged,omitempty"`
}

// ResolveConfig applies defaults and validates the load-time invariants.
func ResolveConfig(config Config) (Config, error) {
	if config.Provider == "" {
		return Config{}, fmt.Errorf("tool-subagent: config provider is required")
	}
	if config.ToolName == "" {
		config.ToolName = "subagent"
	}
	if !config.EnableRunInBackgroundSet {
		config.EnableRunInBackground = true
	}
	switch config.BackgroundMode {
	case "":
		config.BackgroundMode = BackgroundOneShot
	case BackgroundOneShot, BackgroundContinuable:
	default:
		return Config{}, fmt.Errorf("tool-subagent: backgroundMode must be \"one-shot\" or \"continuable\" (got %q)", string(config.BackgroundMode))
	}
	if !config.MaxDepthProviderManaged {
		if err := subagent.AssertSubagentMaxDepth(&config.MaxDepth); err != nil {
			return Config{}, err
		}
	}
	if config.ToolFilter != nil && len(config.ToolFilter.Allow) == 0 && len(config.ToolFilter.Deny) == 0 {
		return Config{}, fmt.Errorf("tool-subagent: `toolFilter` is configured but names neither `allow` nor `deny` — remove the key or fill the filter")
	}
	return config, nil
}

// Deps carries the services the tool composes. Jobs is optional: a one-shot
// background request without it reports the official load hint.
type Deps struct {
	Runtime   *tools.ToolRuntime
	Prompt    *systemprompt.SystemPrompt
	Subagents *subagent.SubagentRuntime
	Jobs      *jobs.LocalRegistry
	Logger    cordisLogger
	// ResolveAgent maps the executing scope key to the live calling agent;
	// a transport sub-dispatch resolves nothing and the tool rejects.
	ResolveAgent func(tools.ScopeKey) *agent.Agent
}

// cordisLogger keeps the dependency face tolerant of nil loggers.
type cordisLogger interface {
	Info(message string)
	Warn(message string)
}

// Register validates the provider composition and mounts the delegation
// tool. The returned disposer unmounts tool and prompt section.
func Register(deps Deps, config Config) (func(), error) {
	if deps.Runtime == nil || deps.Subagents == nil {
		return nil, fmt.Errorf("tool-subagent: the tool runtime and the subagent runtime are required")
	}
	config, err := ResolveConfig(config)
	if err != nil {
		return nil, err
	}
	provider, ok := deps.Subagents.GetProvider(config.Provider)
	if !ok {
		return nil, fmt.Errorf("tool-subagent: subagent provider %q is not registered yet; compose the provider before this tool", config.Provider)
	}
	continuable := config.BackgroundMode == BackgroundContinuable
	if err := assertProviderConfiguration(provider, config, continuable); err != nil {
		return nil, err
	}

	wording := providerWording(provider.InheritsParentContext())
	description := wording.description + backgroundSuffix(config)

	var sectionUndo func()
	if config.EnableRunInBackground && continuable && deps.Prompt != nil {
		sectionUndo, err = deps.Prompt.Section(nil, systemprompt.PromptSection{
			Name:  "tool:" + config.ToolName,
			Order: systemprompt.OrderToolSubagent,
			Text: fmt.Sprintf("Use %s in the background by default. Start independent delegations together in one assistant message and continue useful work while they run. Set `run_in_background: false` only when your next action depends on that subagent's result. "+
				"When a background run settles, the runtime sends you a notice containing its outcome and any final assistant message.", config.ToolName),
		})
		if err != nil {
			return nil, err
		}
	}

	definition, err := tools.DefineTool(tools.DefineToolOptions{
		Name:        config.ToolName,
		Description: description,
		Parameters:  parameterSpec(config, wording),
		Output: tools.ToolOutput{
			Schema: outputSchema(),
			Render: func(args map[string]any, value any) []llm.ContentBlock {
				return []llm.ContentBlock{{Type: llm.BlockText, Text: renderValue(value)}}
			},
		},
		// Children never mutate the parent session; the one parent-owned
		// write is a synchronous commutative insertion.
		IsConcurrencySafe: func(args map[string]any) bool { return true },
		Execute: func(args map[string]any, exec *tools.ToolRunContext) (any, error) {
			return execute(deps, config, continuable, args, exec)
		},
	})
	if err != nil {
		if sectionUndo != nil {
			sectionUndo()
		}
		return nil, err
	}
	unregister, err := deps.Runtime.Register(definition)
	if err != nil {
		if sectionUndo != nil {
			sectionUndo()
		}
		return nil, err
	}
	return func() {
		unregister()
		if sectionUndo != nil {
			sectionUndo()
		}
	}, nil
}

// assertProviderConfiguration rejects a provider that cannot honor the
// configured start-time features, at load rather than at every call.
func assertProviderConfiguration(provider subagent.SubagentProvider, config Config, continuable bool) error {
	capabilities := provider.Capabilities()
	if !config.MaxDepthProviderManaged && !capabilities.DepthLimit {
		return fmt.Errorf(
			"tool-subagent: provider %q cannot enforce maxDepth (no depthLimit capability) — set maxDepth: 'provider-managed' to leave the recursion budget to the provider",
			provider.Name())
	}
	if config.AgentOptions != nil && !capabilities.AgentOptions {
		return fmt.Errorf("tool-subagent: provider %q does not support child agentOptions", provider.Name())
	}
	if continuable {
		if _, ok := provider.(subagent.ContinuableProvider); !ok {
			return fmt.Errorf("tool-subagent: provider %q does not support `backgroundMode: continuable`", provider.Name())
		}
	}
	return nil
}

// backgroundSuffix appends the scheduling wording to the tool description.
func backgroundSuffix(config Config) string {
	if !config.EnableRunInBackground {
		return " This call waits for the subagent and returns its result."
	}
	if config.BackgroundMode == BackgroundContinuable {
		return " This tool runs in the background by default, immediately returns a durable subagent id, and keeps the child conversation available for later turns. When that run settles, the runtime sends the parent a notice containing its outcome and any final assistant message; `send_message` starts a later turn in the same child conversation. Set `run_in_background: false` only when your next action depends on receiving the result."
	}
	return " This call waits for the result by default. Set `run_in_background: true` to return a job id; collect with `job_output` and stop with `job_kill`."
}

// delegationWording is the model-facing description pair.
type delegationWording struct {
	description       string
	promptDescription string
}

// providerWording derives truthful wording from the provider's conversation
// descriptor: a forked child already sees the completed turns.
func providerWording(inheritsConversation bool) delegationWording {
	if inheritsConversation {
		return delegationWording{
			description:       "Delegate a task to a subagent that inherits this conversation: a child agent seeded with all completed turns so far (it does not see the current in-flight turn). Use this when the subtask builds on this conversation's context — a follow-up analysis, a review, a continuation — without consuming this conversation's context for the work itself. You receive its result, not its intermediate steps.",
			promptDescription: "The task for the subagent. It already sees this conversation's completed turns, so build on them freely and state only what is new.",
		}
	}
	return delegationWording{
		description:       "Delegate a self-contained task to a subagent (a separate agent that works in its own context) to offload focused, independent work — research, a scoped implementation, an analysis — so it does not consume this conversation's context. The subagent returns its result, not its intermediate steps. Give it a complete, standalone prompt: it does not see this conversation.",
		promptDescription: "The complete, self-contained task for the subagent. It does not share this conversation's context, so include everything it needs.",
	}
}

// parameterSpec declares the schema. run_in_background is present only when
// background requests are enabled.
func parameterSpec(config Config, wording delegationWording) map[string]tools.PropSpec {
	parameters := map[string]tools.PropSpec{
		"description": {ValueSchemaSpec: tools.ValueSchemaSpec{
			Type:        "string",
			Description: "A short (3-5 word) description of the delegated task, for display.",
		}, Required: true},
		"prompt": {ValueSchemaSpec: tools.ValueSchemaSpec{
			Type:        "string",
			Description: wording.promptDescription,
		}, Required: true},
	}
	if config.EnableRunInBackground {
		text := "Whether to run as a background job and return its id. Defaults to false; collect with job_output or stop with job_kill."
		if config.BackgroundMode == BackgroundContinuable {
			text = "Whether to run in the background and return a durable subagent id immediately. Defaults to true. Set false to wait for the result when your next action depends on it."
		}
		parameters["run_in_background"] = tools.PropSpec{ValueSchemaSpec: tools.ValueSchemaSpec{
			Type:        "boolean",
			Description: text,
		}}
	}
	return parameters
}

// outputSchema declares the three terminal shapes.
func outputSchema() *tools.ValueSchemaSpec {
	return &tools.ValueSchemaSpec{
		OneOf: []*tools.ValueSchemaSpec{
			{
				Type:                 "object",
				AdditionalProperties: boolPtr(false),
				Properties: map[string]tools.PropSpec{
					"kind":  {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string", Const: "background"}, Required: true},
					"jobId": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}, Required: true},
				},
			},
			{
				Type:                 "object",
				AdditionalProperties: boolPtr(false),
				Properties: map[string]tools.PropSpec{
					"kind":       {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string", Const: "continuable"}, Required: true},
					"subagentId": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}, Required: true},
				},
			},
			{
				Type:                 "object",
				AdditionalProperties: boolPtr(false),
				Properties: map[string]tools.PropSpec{
					"kind":   {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string", Const: "foreground"}, Required: true},
					"runId":  {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}, Required: true},
					"output": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "array", Items: &tools.ValueSchemaSpec{Type: "json"}}, Required: true},
				},
			},
		},
	}
}

func boolPtr(b bool) *bool { return &b }

// renderValue projects the canonical value into model-facing text.
func renderValue(value any) string {
	outcome, ok := value.(map[string]any)
	if !ok {
		return fmt.Sprintf("%v", value)
	}
	switch kind, _ := outcome["kind"].(string); kind {
	case "background":
		return fmt.Sprintf("started background subagent job %v", outcome["jobId"])
	case "continuable":
		return fmt.Sprintf("started subagent %v", outcome["subagentId"])
	default:
		blocks, _ := outcome["output"].([]llm.ContentBlock)
		var text strings.Builder
		for _, block := range blocks {
			if block.Type == llm.BlockText {
				text.WriteString(block.Text)
			}
		}
		return text.String()
	}
}

// delegationArgs is the validated model argument face.
type delegationArgs struct {
	description     string
	prompt          string
	runInBackground bool
}

// resolveDelegationRun resolves the model's optional scheduling request into
// one execution route.
func resolveDelegationRun(args delegationArgs, backgroundEnabled bool, continuable bool) (bool, error) {
	if !backgroundEnabled {
		if args.runInBackground {
			return false, fmt.Errorf("run_in_background is disabled for this tool instance (enableRunInBackground: false)")
		}
		return false, nil
	}
	if args.runInBackground {
		return true, nil
	}
	// Continuable work is independently scheduled unless the caller needs
	// the result first; one-shot keeps its foreground default.
	return continuable, nil
}

// execute is the tool body: start the child through the configured route.
func execute(deps Deps, config Config, continuable bool, args map[string]any, exec *tools.ToolRunContext) (any, error) {
	parent := deps.ResolveAgent(exec.Agent)
	if parent == nil {
		return nil, fmt.Errorf("subagent tool requires a calling agent (exec.agent was undefined)")
	}
	parsed, err := parseArgs(args)
	if err != nil {
		return nil, err
	}
	background, err := resolveDelegationRun(parsed, config.EnableRunInBackground, continuable)
	if err != nil {
		return nil, err
	}

	request := subagent.SubagentStartRequest{
		Label:  parsed.description,
		Prompt: []llm.ContentBlock{{Type: llm.BlockText, Text: parsed.prompt}},
		Parent: parent,
		Signal: exec.Signal,
	}
	if config.AgentOptions != nil {
		request.AgentOptions = config.AgentOptions
	}
	if config.Persona != "" {
		request.Persona = config.Persona
	}
	if config.ToolFilter != nil {
		request.ToolFilter = &subagent.ToolRestriction{Allow: config.ToolFilter.Allow, Deny: config.ToolFilter.Deny}
	}
	if !config.MaxDepthProviderManaged {
		maxDepth := config.MaxDepth
		request.MaxDepth = &maxDepth
	}

	if background {
		if continuable {
			started, err := deps.Subagents.StartContinuable(subagent.ContinuableStartSpec{
				Provider: config.Provider,
				Label:    parsed.description,
				Request:  continuableRequestOf(request),
				Signal:   exec.Signal,
			})
			if err != nil {
				return nil, err
			}
			return map[string]any{"kind": "continuable", "subagentId": string(started.ChildID)}, nil
		}
		if deps.Jobs == nil {
			return nil, fmt.Errorf("background jobs unavailable: load @deepseek-ai/dsh-jobs and @deepseek-ai/dsh-tool-jobs")
		}
		jobID, err := deps.Jobs.Start(jobs.StartSpec{
			Kind:  "subagent",
			Label: parsed.description,
			Owner: jobOwner{agent: parent},
			Run: func() (jobs.Hooks, error) {
				return startBackgroundRun(deps, config, request)
			},
		})
		if err != nil {
			return nil, err
		}
		return map[string]any{"kind": "background", "jobId": jobID}, nil
	}

	run, err := deps.Subagents.Start(config.Provider, request)
	if err != nil {
		return nil, err
	}
	return settleForegroundRun(run)
}

// parseArgs validates and extracts the model arguments.
func parseArgs(args map[string]any) (delegationArgs, error) {
	description, _ := args["description"].(string)
	prompt, _ := args["prompt"].(string)
	if description == "" {
		return delegationArgs{}, fmt.Errorf("description is required")
	}
	if prompt == "" {
		return delegationArgs{}, fmt.Errorf("prompt is required")
	}
	runInBackground, _ := args["run_in_background"].(bool)
	return delegationArgs{description: description, prompt: prompt, runInBackground: runInBackground}, nil
}

// continuableRequestOf projects the start request onto the continuable
// delegation request (same feature fields minus the one-shot signal channel).
func continuableRequestOf(request subagent.SubagentStartRequest) subagent.ContinuableDelegationRequest {
	return subagent.ContinuableDelegationRequest{
		Prompt:       request.Prompt,
		Parent:       request.Parent,
		AgentOptions: request.AgentOptions,
		Persona:      request.Persona,
		ToolFilter:   request.ToolFilter,
		MaxDepth:     request.MaxDepth,
	}
}

// startBackgroundRun opens the one-shot background task: job preflight
// finishes before the starter spawns, and the task-owned signal covers
// startup.
func startBackgroundRun(deps Deps, config Config, request subagent.SubagentStartRequest) (jobs.Hooks, error) {
	controller, cancel := context.WithCancel(context.Background())
	request.Signal = controller
	done := make(chan jobs.Result, 1)
	go func() {
		run, err := deps.Subagents.Start(config.Provider, request)
		if err != nil {
			// Product providers aggregate startup and rollback failures;
			// keep the failure text as the job detail.
			outcome := subagent.JobOutcome{Status: subagent.JobStatusFailed, Detail: err.Error()}
			if controller.Err() != nil {
				outcome = subagent.JobOutcome{Status: subagent.JobStatusKilled}
			}
			done <- jobs.Result{Outcome: outcomeOf(outcome)}
			return
		}
		outcome := subagent.SettleRun(run)
		if err := run.Dispose(); err != nil {
			done <- jobs.Result{Err: fmt.Errorf("subagent run failed: %s; dispose failed: %v", outcome.Status, err)}
			return
		}
		done <- jobs.Result{Outcome: outcomeOf(outcome)}
	}()
	return jobs.Hooks{
		Cancel: func(reason string) error {
			cancel()
			return nil
		},
		Done: done,
	}, nil
}

// outcomeOf bridges the subagent and jobs outcome vocabularies (identical
// status strings, kind-specific detail/output fields).
func outcomeOf(outcome subagent.JobOutcome) jobs.Outcome {
	return jobs.Outcome{Status: outcome.Status, Detail: outcome.Detail, Output: outcome.Output}
}

// jobOwner adapts the live calling agent onto the jobs ownership face.
type jobOwner struct{ agent *agent.Agent }

func (o jobOwner) OwnerID() string            { return string(o.agent.ID) }
func (o jobOwner) OwnerScope() scope.ScopeKey { return o.agent.Scope }

// settleForegroundRun collects and releases one foreground run without
// letting disposal replace an independent result failure.
func settleForegroundRun(run subagent.SubagentRun) (any, error) {
	result, execErr := run.Result()
	disposeErr := run.Dispose()
	if execErr != nil {
		if disposeErr != nil {
			return nil, fmt.Errorf("subagent run failed: %v; dispose failed: %v", execErr, disposeErr)
		}
		return nil, execErr
	}
	if disposeErr != nil {
		return nil, disposeErr
	}
	if err := stopReasonError(result); err != "" {
		return nil, fmt.Errorf("%s", withDiagnosticAndPartialText(err, result))
	}
	return map[string]any{
		"kind":   "foreground",
		"runId":  run.ID(),
		"output": result.Output,
	}, nil
}

// stopReasonError maps a non-completed terminal reason to its headline.
func stopReasonError(result subagent.SubagentResult) string {
	switch result.StopReason {
	case subagent.StopCompleted:
		return ""
	case subagent.StopAborted:
		return "subagent run was cancelled"
	case subagent.StopError:
		return "subagent run failed"
	case subagent.StopMaxTokens:
		return "subagent run hit its token limit before finishing"
	case subagent.StopRefusal:
		return "subagent declined the task"
	default:
		return fmt.Sprintf("subagent run ended abnormally (%s)", string(result.StopReason))
	}
}

// withDiagnosticAndPartialText appends provider-authored failure detail and
// the child's preserved partial answer to a stop-reason error.
func withDiagnosticAndPartialText(errLine string, result subagent.SubagentResult) string {
	diagnostic := ""
	if result.Diagnostic != "" {
		diagnostic = "\nDiagnostic: " + result.Diagnostic
	}
	text := ""
	for _, block := range result.Output {
		if block.Type == llm.BlockText {
			text += block.Text
		}
	}
	partial := ""
	if text != "" {
		partial = "\nPartial output before the run ended:\n" + text
	}
	return errLine + diagnostic + partial
}

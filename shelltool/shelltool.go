// Package shelltool ports @deepseek-ai/dsh-tool-bash + dsh-tool-pwsh: the
// model-facing bash/pwsh tool over the shell executor seam. One registration
// per composed executor flavor; the tool name, prompt section, and
// description follow the executor.
package shelltool

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"dshgo/agent"
	"dshgo/jobs"
	"dshgo/llm"
	"dshgo/scope"
	"dshgo/shell"
	"dshgo/subprocess"
	"dshgo/systemprompt"
	"dshgo/tools"
)

// Config carries the tool-plugin surface (official Config).
type Config struct {
	// BackgroundEnabled exposes `run_in_background` (default true);
	// disabled calls are also rejected at execute time.
	BackgroundEnabled bool
}

// DefaultConfig returns the official default.
func DefaultConfig() Config { return Config{BackgroundEnabled: true} }

// Deps carries the composed services the tool injects.
type Deps struct {
	// Runtime is the shared tool registry.
	Runtime *tools.ToolRuntime
	// Prompt receives the cross-call guidance section (optional).
	Prompt *systemprompt.SystemPrompt
	// Shell is the composed executor (exactly one per host).
	Shell shell.ShellExecutor
	// Env is the managed DSH_* registry.
	Env *shell.ShellEnvRegistry
	// Jobs is optional: background starts need it, foreground runs do not.
	Jobs *jobs.LocalRegistry
	// Agents resolves the calling agent (optional; session-cwd defaulting
	// and background ownership use it).
	Agents func(scope tools.ScopeKey) *agent.Agent
}

// toolName maps the executor flavor onto its tool identity.
func toolName(e shell.ShellExecutor) string {
	if e.Name() == "pwsh-local" {
		return "pwsh"
	}
	return "bash"
}

const bashGuidance = "Check the [exit code: N] marker on every bash result; investigate failures before moving on."

const pwshGuidance = "Non-zero exits are reported as `[exit code: N]` markers; investigate failures before moving on. " +
	"On Windows a killed process settles as `[exit code: 1]` without a signal marker; treat a bare exit 1 after an interruption as a termination, not a command failure."

// toolDescription rebuilds the model-facing description. The escalation
// tail stays off: the Go composition mounts no confining shell executor,
// so the fields are unadvertised and the markers unreachable (an honest
// composition fact, not a wording gap). The sandbox clause follows the
// executor's SandboxMode the same way — it is only promised when the
// composed executor actually confines.
func toolDescription(e shell.ShellExecutor, background bool) string {
	var backgroundClause string
	if background {
		backgroundClause = "Set `run_in_background: true` for long-running commands: the call returns a job id immediately; read its output with `job_output` and stop it with `job_kill`."
	} else {
		backgroundClause = "Background execution is not available; long-running commands must finish within the timeout."
	}
	base := ""
	if toolName(e) == "pwsh" {
		base = "Execute a PowerShell command (`pwsh -Command`) and return its stdout/stderr. "
	} else {
		base = "Execute a bash command (`bash -c`) and return its stdout/stderr. "
	}
	base += "Each call runs in a fresh shell: no state (cwd, variables, functions) persists between calls — " +
		"pass `workdir` instead of using `cd`. Non-zero exits are reported as `[exit code: N]`. " +
		"Current harness environment facts are exposed through managed `$DSH_*` variables; inspect them when needed. "
	if e.SandboxMode() != "" {
		base += "Commands run under a file sandbox; a blocked file operation is reported as `[sandbox: file access denied under <mode> mode]` — a policy denial, not a bug in the command; do not retry another way. "
	}
	base += "Long output is truncated to its tail; the full output is saved to a file whose path is reported when available. " +
		backgroundClause
	return base
}

// Register adds the model-facing shell tool and its prompt section. The
// returned func unregisters both.
func Register(deps Deps, cfg Config) (func(), error) {
	name := toolName(deps.Shell)
	section := systemprompt.PromptSection{Name: "tool:" + name, Text: bashGuidance}
	if name == "pwsh" {
		section.Name = "tool:pwsh"
		section.Order = systemprompt.OrderToolPwsh
		section.Text = pwshGuidance
	} else {
		section.Order = systemprompt.OrderToolBash
	}
	var sectionUndo func()
	if deps.Prompt != nil {
		undo, err := deps.Prompt.Section(nil, section)
		if err != nil {
			return nil, err
		}
		sectionUndo = undo
	}
	rollback := func() {
		if sectionUndo != nil {
			sectionUndo()
		}
	}

	definition, err := tools.DefineTool(tools.DefineToolOptions{
		Name:        name,
		Description: toolDescription(deps.Shell, cfg.BackgroundEnabled),
		Parameters:  parameterSpec(cfg),
		Output: tools.ToolOutput{
			Schema: outputSchema(),
			Render: func(args map[string]any, value any) []llm.ContentBlock {
				outcome, ok := value.(map[string]any)
				if !ok {
					return []llm.ContentBlock{{Type: llm.BlockText, Text: fmt.Sprintf("%v", value)}}
				}
				if kind, _ := outcome["kind"].(string); kind == "background" {
					jobID, _ := outcome["jobId"].(string)
					return []llm.ContentBlock{{Type: llm.BlockText, Text: "started background job " + jobID}}
				}
				return []llm.ContentBlock{{Type: llm.BlockText, Text: renderForeground(outcome)}}
			},
		},
		Execute: func(args map[string]any, exec *tools.ToolRunContext) (any, error) {
			return deps.execute(args, exec, cfg)
		},
	})
	if err != nil {
		rollback()
		return nil, err
	}
	undoTool, err := deps.Runtime.Register(definition)
	if err != nil {
		rollback()
		return nil, err
	}
	return func() {
		undoTool()
		if sectionUndo != nil {
			sectionUndo()
		}
	}, nil
}

func parameterSpec(cfg Config) map[string]tools.PropSpec {
	params := map[string]tools.PropSpec{
		"command": {ValueSchemaSpec: tools.ValueSchemaSpec{
			Type:        "string",
			Description: "The command to execute.",
		}, Required: true},
		"description": {ValueSchemaSpec: tools.ValueSchemaSpec{
			Type: "string",
			Description: "Clear, concise description of what this command does in active voice, " +
				"5-10 words (shown in the UI). Examples: \"ls\" → \"List files in current directory\"; " +
				"\"git status\" → \"Show working tree status\".",
		}, Required: true},
		"timeoutMs": {ValueSchemaSpec: tools.ValueSchemaSpec{
			Type:        "number",
			Description: "Timeout in milliseconds. The executor applies its configured default and cap, and kills the command on expiry.",
		}},
		"workdir": {ValueSchemaSpec: tools.ValueSchemaSpec{
			Type:        "string",
			Description: "Working directory for this command. Defaults to the session workspace; a relative path is resolved against it.",
		}},
	}
	if cfg.BackgroundEnabled {
		params["run_in_background"] = tools.PropSpec{ValueSchemaSpec: tools.ValueSchemaSpec{
			Type:        "boolean",
			Description: "Run in the background and return a job id immediately (collect with job_output, stop with job_kill). No timeout applies.",
		}}
	}
	return params
}

// outputSchema declares the background acknowledgement / foreground result
// union. The sandbox result branch stays unadvertised: no confining shell
// executor is composed.
func outputSchema() *tools.ValueSchemaSpec {
	stream := func() tools.PropSpec {
		return tools.PropSpec{ValueSchemaSpec: tools.ValueSchemaSpec{
			Type:                 "object",
			AdditionalProperties: boolPtr(false),
			Properties: map[string]tools.PropSpec{
				"text":      {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}, Required: true},
				"truncated": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "boolean"}, Required: true},
				"spillPath": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}},
			},
		}, Required: true}
	}
	background := tools.ValueSchemaSpec{
		Type:                 "object",
		AdditionalProperties: boolPtr(false),
		Properties: map[string]tools.PropSpec{
			"kind":  {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string", Const: "background"}, Required: true},
			"jobId": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}, Required: true},
		},
	}
	foreground := tools.ValueSchemaSpec{
		Type:                 "object",
		AdditionalProperties: boolPtr(false),
		Properties: map[string]tools.PropSpec{
			"kind":      {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string", Const: "foreground"}, Required: true},
			"exitCode":  {ValueSchemaSpec: tools.ValueSchemaSpec{OneOf: []*tools.ValueSchemaSpec{{Type: "integer"}, {Type: "null"}}}, Required: true},
			"signal":    {ValueSchemaSpec: tools.ValueSchemaSpec{OneOf: []*tools.ValueSchemaSpec{{Type: "string"}, {Type: "null"}}}, Required: true},
			"timedOut":  {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "boolean"}, Required: true},
			"aborted":   {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "boolean"}, Required: true},
			"timeoutMs": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "number"}, Required: true},
			"stdout":    stream(),
			"stderr":    stream(),
		},
	}
	return &tools.ValueSchemaSpec{OneOf: []*tools.ValueSchemaSpec{&background, &foreground}}
}

func boolPtr(v bool) *bool { return &v }

// argString reads one string argument (absent → "").
func argString(args map[string]any, key string) string {
	raw, _ := args[key].(string)
	return raw
}

// argNumber reads one numeric argument (absent → 0).
func argNumber(args map[string]any, key string) int {
	switch v := args[key].(type) {
	case int:
		return v
	case float64:
		return int(v)
	}
	return 0
}

// validateBashArgs enforces the value constraints the parameter schema
// cannot express (official validateBashArgs, minus the escalation pairing
// this composition does not advertise).
func validateBashArgs(args map[string]any) error {
	if strings.TrimSpace(argString(args, "command")) == "" {
		return errors.New("invalid command: expected a non-empty string")
	}
	if strings.TrimSpace(argString(args, "description")) == "" {
		return errors.New("invalid description: expected a non-empty string")
	}
	if raw, has := args["timeoutMs"]; has {
		number, ok := raw.(float64)
		if !ok && !strings.HasPrefix(fmt.Sprintf("%T", raw), "int") {
			return errors.New("invalid timeoutMs: expected a positive number")
		}
		if (ok && (number <= 0)) || (!ok && argNumber(args, "timeoutMs") <= 0) {
			return fmt.Errorf("invalid timeoutMs: expected a positive number, got %v", raw)
		}
	}
	return nil
}

// resolveWorkdir applies the session-workspace defaulting: an explicit
// relative model workdir resolves against the session cwd; absent, the
// session cwd stands (executor config defaulting remains the fallback).
func (d Deps) resolveWorkdir(modelWorkdir string, exec *tools.ToolRunContext) string {
	sessionCwd := d.sessionCwd(exec)
	if modelWorkdir == "" {
		return sessionCwd
	}
	if sessionCwd != "" && !filepath.IsAbs(modelWorkdir) {
		return filepath.Join(sessionCwd, modelWorkdir)
	}
	return modelWorkdir
}

func (d Deps) sessionCwd(exec *tools.ToolRunContext) string {
	if d.Agents == nil || exec.Agent == nil {
		return ""
	}
	caller := d.Agents(exec.Agent)
	if caller == nil || caller.Session == nil {
		return ""
	}
	return caller.Session.Header().CWD
}

func (d Deps) execute(args map[string]any, exec *tools.ToolRunContext, cfg Config) (any, error) {
	if err := validateBashArgs(args); err != nil {
		return nil, err
	}
	workdir := d.resolveWorkdir(argString(args, "workdir"), exec)
	dshEnv := d.Env.Collect(&exec.ToolExecution)
	request := shell.ShellExecRequest{
		Command:   argString(args, "command"),
		Workdir:   workdir,
		TimeoutMs: argNumber(args, "timeoutMs"),
		DshEnv:    dshEnv,
	}
	backgroundRequested := false
	if raw, has := args["run_in_background"]; has {
		if flag, ok := raw.(bool); ok {
			backgroundRequested = flag
		}
	}
	if backgroundRequested {
		if !cfg.BackgroundEnabled {
			return nil, errors.New("run_in_background is disabled for this deployment (enableRunInBackground: false)")
		}
		if d.Jobs == nil {
			return nil, errors.New("background jobs unavailable: load @deepseek-ai/dsh-jobs and @deepseek-ai/dsh-tool-jobs")
		}
		// The caller owns cancellation until ctx.jobs commits detached
		// ownership.
		if exec.Signal != nil && exec.Signal.Err() != nil {
			return nil, errors.New("tool call aborted")
		}
		jobID, err := d.Jobs.Start(jobs.StartSpec{
			Kind:  "bash",
			Label: request.Command,
			Owner: d.ownerOf(exec),
			Run: func() (jobs.Hooks, error) {
				return d.startBackground(request)
			},
		})
		if err != nil {
			return nil, err
		}
		return map[string]any{"kind": "background", "jobId": jobID}, nil
	}
	signal := exec.Signal
	if signal == nil {
		signal = context.Background()
	}
	result, err := d.Shell.Run(d.Shell.Resolve(func() shell.ShellExecRequest {
		request.Signal = signal
		return request
	}()))
	if err != nil {
		return nil, err
	}
	if result.Aborted {
		return nil, errors.New("tool call aborted")
	}
	return canonicalForeground(result), nil
}

// ownerOf adapts the calling agent onto the jobs ownership face; an
// agent-less call creates an unowned job.
func (d Deps) ownerOf(exec *tools.ToolRunContext) jobs.Owner {
	if d.Agents == nil || exec.Agent == nil {
		return nil
	}
	caller := d.Agents(exec.Agent)
	if caller == nil {
		return nil
	}
	return agentOwner{id: string(caller.ID), sc: caller.Scope}
}

type agentOwner struct {
	id string
	sc scope.ScopeKey
}

func (o agentOwner) OwnerID() string            { return o.id }
func (o agentOwner) OwnerScope() scope.ScopeKey { return o.sc }

// startBackground spawns the detached process and maps its lifecycle onto
// the generic job hooks. The job owns a private cancellation context: the
// spec's signal carries the stop to the executor (and its process tree),
// and the Cancel hook fires it.
func (d Deps) startBackground(request shell.ShellExecRequest) (jobs.Hooks, error) {
	stop, cancel := context.WithCancel(context.Background())
	request.Signal = stop
	spec := d.Shell.Resolve(request)
	proc, err := d.Shell.Start(spec)
	if err != nil {
		cancel()
		return jobs.Hooks{}, err
	}
	result := make(chan jobs.Result, 1)
	go func() {
		defer cancel()
		<-proc.Done()
		status, detail := processOutcome(proc)
		result <- jobs.Result{Outcome: jobs.Outcome{Status: status, Detail: detail}}
	}()
	return jobs.Hooks{
		Cancel: func(string) error {
			cancel()
			return nil
		},
		Done:       result,
		ReadOutput: func() string { return RenderProcessRead(proc.ReadOutput()) },
	}, nil
}

// processOutcome maps a settled background process onto the generic
// task-outcome vocabulary: killed stays killed (detail: the signal when one
// is known), everything else is completed with the exit code as detail. A
// nonzero command exit is reported, not failed, exactly like the
// foreground rendering.
func processOutcome(proc shell.ShellProcess) (string, string) {
	if proc.Status() == shell.ProcessKilled {
		if proc.Signal() != "" {
			return jobs.OutcomeKilled, "signal: " + proc.Signal()
		}
		return jobs.OutcomeKilled, "killed before exit"
	}
	return jobs.OutcomeCompleted, fmt.Sprintf("exit code %d", proc.ExitCode())
}

// canonicalForeground detaches the executor DTO into plain lossless JSON.
func canonicalForeground(result shell.ShellRunResult) map[string]any {
	output := func(stream subprocess.CollectedOutput) map[string]any {
		out := map[string]any{"text": stream.Text, "truncated": stream.Truncated}
		if stream.SpillPath != "" {
			out["spillPath"] = stream.SpillPath
		}
		return out
	}
	exitCode := any(result.ExitCode)
	signal := any(nil)
	if result.Signal != "" {
		signal = result.Signal
	} else if result.ExitCode < 0 {
		exitCode = nil
	}
	return map[string]any{
		"kind":      "foreground",
		"exitCode":  exitCode,
		"signal":    signal,
		"timedOut":  result.TimedOut,
		"aborted":   result.Aborted,
		"timeoutMs": result.TimeoutMs,
		"stdout":    output(result.Stdout),
		"stderr":    output(result.Stderr),
	}
}

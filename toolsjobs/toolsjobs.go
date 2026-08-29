// Package toolsjobs ports @deepseek-ai/dsh-tool-jobs: the model-facing
// `job_output`, `job_list`, and `job_kill` tools over the jobs registry.
// Registering the tools attaches the controller producers require and
// contributes the cross-call guidance section to the system prompt. It also
// delivers unreported completions to the owning agent through the host's
// delivery seam, bounded per owner.
package toolsjobs

import (
	"fmt"
	"strings"
	"sync"

	"dshgo/jobs"
	"dshgo/llm"
	"dshgo/outputretention"
	"dshgo/systemprompt"
	"dshgo/tools"
)

// Defaults for Config.
const (
	DefaultWaitTimeoutMs    = 30_000
	DefaultMaxWaitTimeoutMs = 600_000
	DefaultMaxWakes         = 3
)

// CompletionDelivery values.
const (
	DeliveryQuiet  = "quiet"
	DeliveryWakeup = "wakeup"
)

// System prompt section contributed by the tools.
const (
	SectionName = "tool:jobs"
	SectionText = "Track every background job id you start. You are notified in-session when a job finishes — do not busy-poll or sleep on one; keep working on independent steps and do not duplicate a running job's work. Before giving a final answer, collect every still-relevant job with job_output (set wait: true only when you are genuinely blocked on it), and job_kill jobs that stopped mattering."
)

// Config configures bounded job_output waits and completion-notice
// delivery.
type Config struct {
	// WaitTimeoutMs is the wait duration applied when job_output sets
	// wait without timeout_ms.
	WaitTimeoutMs float64
	// MaxWaitTimeoutMs is the hard cap on any single wait; a larger
	// model-supplied timeout_ms is clamped down to it.
	MaxWaitTimeoutMs float64
	// CompletionDelivery is how an unreported completion reaches an owner
	// that is already idle: wakeup opens a turn for it, quiet leaves it
	// pending until something else wakes the owner. A busy owner is
	// injected either way.
	CompletionDelivery string
	// MaxConsecutiveWakes bounds the turns one owner may have opened by
	// completion wakes before the next notice degrades to injection,
	// reset by any user-authored input.
	MaxConsecutiveWakes int
}

// ResolveConfig applies defaults, validates the bounds, and fails loud.
func ResolveConfig(config Config) (Config, error) {
	if config.WaitTimeoutMs == 0 {
		config.WaitTimeoutMs = DefaultWaitTimeoutMs
	}
	if config.MaxWaitTimeoutMs == 0 {
		config.MaxWaitTimeoutMs = DefaultMaxWaitTimeoutMs
	}
	if config.CompletionDelivery == "" {
		config.CompletionDelivery = DeliveryWakeup
	}
	if config.MaxConsecutiveWakes == 0 {
		config.MaxConsecutiveWakes = DefaultMaxWakes
	}
	if config.WaitTimeoutMs < 1 || config.WaitTimeoutMs != config.WaitTimeoutMs {
		return Config{}, fmt.Errorf("tool-jobs: waitTimeoutMs (%v) must be a number >= 1", config.WaitTimeoutMs)
	}
	if config.MaxWaitTimeoutMs < 1 || config.MaxWaitTimeoutMs != config.MaxWaitTimeoutMs {
		return Config{}, fmt.Errorf("tool-jobs: maxWaitTimeoutMs (%v) must be a number >= 1", config.MaxWaitTimeoutMs)
	}
	if config.WaitTimeoutMs > config.MaxWaitTimeoutMs {
		return Config{}, fmt.Errorf("tool-jobs: waitTimeoutMs (%v) exceeds maxWaitTimeoutMs (%v)", config.WaitTimeoutMs, config.MaxWaitTimeoutMs)
	}
	if config.CompletionDelivery != DeliveryQuiet && config.CompletionDelivery != DeliveryWakeup {
		return Config{}, fmt.Errorf("tool-jobs: completionDelivery must be 'quiet' or 'wakeup'")
	}
	if config.MaxConsecutiveWakes < 1 {
		// A budget is a count of turns: a fraction never names a turn at
		// all, and zero would starve every wake.
		return Config{}, fmt.Errorf("tool-jobs: maxConsecutiveWakes (%v) must be a whole number of turns", config.MaxConsecutiveWakes)
	}
	return config, nil
}

// CallerOf resolves the owning agent's session id for one execution — the
// Go adaptation of exec.agent, whose runtime routes by scope key rather
// than a live agent face. Nil resolves every caller to the empty session,
// which sees only unowned jobs.
type CallerOf func(exec *tools.ToolExecution) string

// PublicJobSnapshot is task state safe for model-authored programs;
// ownership and notification bookkeeping are omitted.
type PublicJobSnapshot struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Label      string `json:"label"`
	Status     string `json:"status"`
	Detail     string `json:"detail,omitempty"`
	StartedAt  int64  `json:"startedAt"`
	FinishedAt int64  `json:"finishedAt,omitempty"`
}

// PublicJob removes job ownership and notification bookkeeping from a
// registry snapshot.
func PublicJob(snapshot jobs.Snapshot) PublicJobSnapshot {
	return PublicJobSnapshot{
		ID:         snapshot.ID,
		Kind:       snapshot.Kind,
		Label:      snapshot.Label,
		Status:     snapshot.Status,
		Detail:     snapshot.Detail,
		StartedAt:  snapshot.StartedAt,
		FinishedAt: snapshot.FinishedAt,
	}
}

// StatusLine renders generic status with optional producer detail.
func StatusLine(status, detail string) string {
	if detail != "" {
		return fmt.Sprintf("[status: %s, %s]", status, detail)
	}
	return fmt.Sprintf("[status: %s]", status)
}

func retainTail(text string, maxBytes int) string {
	retainer := outputretention.NewTextRetainer(outputretention.TextStrategy{Kind: "tail", MaxBytes: maxBytes})
	retainer.PushString(text)
	return retainer.Finish().Text
}

func retainHead(text string, maxBytes int) string {
	retainer := outputretention.NewTextRetainer(outputretention.TextStrategy{Kind: "head", MaxBytes: maxBytes})
	retainer.PushString(text)
	return retainer.Finish().Text
}

// FitWithSuffix bounds content plus a suffix to maxBytes (zero = unset).
// Over budget, the omission marker is promoted into the fixed suffix so
// only the content shrinks; when even that cannot fit, the tail survives.
func FitWithSuffix(content, suffix string, maxBytes int, omitted string) string {
	complete := content + suffix
	if maxBytes == 0 || len(complete) <= maxBytes {
		return complete
	}
	fixed := omitted + suffix
	if strings.HasSuffix(content, strings.TrimLeft(omitted, " \t")) {
		fixed = suffix
	}
	fixedBytes := len(fixed)
	if fixedBytes >= maxBytes {
		return retainTail(fixed, maxBytes)
	}
	return retainTail(content, maxBytes-fixedBytes) + fixed
}

// CompletionSummary is the one-line account of a settled job for the
// notice form's collapsed row, bounded like every notice summary.
func CompletionSummary(snapshot jobs.Snapshot) string {
	return llm.BoundContextSummary(fmt.Sprintf("%s %s %s", snapshot.Kind, snapshot.Label, StatusLine(snapshot.Status, snapshot.Detail)))
}

// FitCompletionNotice formats the completion notice for one settled job
// under the producer's output limit: the full sentence, then the truncated
// variant with a `[notice truncated]` marker, then the compact prefix, then
// the action line alone.
func FitCompletionNotice(snapshot jobs.Snapshot) string {
	prefix := fmt.Sprintf("background job %s", snapshot.ID)
	detail := fmt.Sprintf(" (%s: %s) finished %s", snapshot.Kind, snapshot.Label, StatusLine(snapshot.Status, snapshot.Detail))
	action := "\nDone; job_output."
	complete := fmt.Sprintf("%s%s. Read its output with job_output.", prefix, detail)
	maxBytes := snapshot.OutputLimitBytes
	if maxBytes == 0 || len(complete) <= maxBytes {
		return complete
	}
	omitted := "\n[notice truncated]"
	fixed := prefix + omitted + action
	fixedBytes := len(fixed)
	if fixedBytes <= maxBytes {
		if fixedBytes == maxBytes {
			return fixed
		}
		return prefix + retainHead(detail, maxBytes-fixedBytes) + omitted + action
	}
	compact := prefix + action
	compactBytes := len(compact)
	if compactBytes <= maxBytes {
		return compact
	}
	actionBytes := len(action)
	if actionBytes >= maxBytes {
		return retainTail(action, maxBytes)
	}
	return retainHead(prefix, maxBytes-actionBytes) + action
}

// publicJobValue projects one snapshot into the canonical lossless-JSON
// value the tool pipeline carries, omitting absent optional fields exactly
// like the official publicJob spread.
func publicJobValue(snapshot jobs.Snapshot) map[string]any {
	value := map[string]any{
		"id":        snapshot.ID,
		"kind":      snapshot.Kind,
		"label":     snapshot.Label,
		"status":    snapshot.Status,
		"startedAt": snapshot.StartedAt,
	}
	if snapshot.Detail != "" {
		value["detail"] = snapshot.Detail
	}
	if snapshot.FinishedAt != 0 {
		value["finishedAt"] = snapshot.FinishedAt
	}
	return value
}

// outputValue is job_output's canonical value.
func outputValue(text string, job map[string]any) map[string]any {
	return map[string]any{"text": text, "job": job}
}

// killValue is job_kill's canonical value; outcome is
// cancellation-requested or already-finished.
func killValue(outcome string, job map[string]any) map[string]any {
	return map[string]any{"outcome": outcome, "job": job}
}

// validateJobID enforces the non-empty constraint the parameter schema
// cannot express.
func validateJobID(value any) (string, error) {
	id, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("invalid job_id: expected a string")
	}
	if len(id) == 0 {
		return "", fmt.Errorf("invalid job_id: expected a non-empty string, got %q", id)
	}
	return id, nil
}

// StatusEnum is the shared status enum for job-control outputs.
var StatusEnum = []any{jobs.StatusRunning, jobs.StatusStopping, jobs.StatusCompleted, jobs.StatusKilled, jobs.StatusFailed}

// closedObject is the explicit closed-object choice the schema compiler
// requires.
func closedObject() *bool { closed := false; return &closed }

// publicTaskSchema is the shared schema for job-control outputs, mirrored
// by PublicJobSnapshot's JSON tags.
func publicTaskSchema() *tools.ValueSchemaSpec {
	return &tools.ValueSchemaSpec{
		Type:                 "object",
		AdditionalProperties: closedObject(),
		Properties: map[string]tools.PropSpec{
			"id":         {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}, Required: true},
			"kind":       {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}, Required: true},
			"label":      {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}, Required: true},
			"status":     {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string", Enum: StatusEnum}, Required: true},
			"detail":     {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}},
			"startedAt":  {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "integer"}, Required: true},
			"finishedAt": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "integer"}},
		},
	}
}

// pendingTaskView is the pending presentation shared by the three generic
// job controls.
func pendingTaskView(title, kind, rawInput string) map[string]any {
	view := map[string]any{"card": "generic", "title": title, "kind": kind}
	if rawInput != "" {
		view["rawInput"] = rawInput
	}
	return view
}

// boundedSingleText collapses model-facing content to one text block under
// maxBytes; nil passes non-single-text content through untouched.
func boundedSingleText(content []llm.ContentBlock, maxBytes int) []llm.ContentBlock {
	if len(content) != 1 || content[0].Type != "text" {
		return nil
	}
	return []llm.ContentBlock{{
		Type: "text",
		Text: FitWithSuffix(content[0].Text, "", maxBytes, "\n[result truncated]"),
	}}
}

// RegisterTools defines and registers the job-control tools onto one tool
// runtime and attaches the controller producers require. resolve maps an
// execution to its owning agent's session id; nil resolves the empty
// caller. The returned disposer detaches the controller.
func RegisterTools(runtime *tools.ToolRuntime, registry *jobs.LocalRegistry, resolve CallerOf, config Config) (func(), error) {
	resolved, err := ResolveConfig(config)
	if err != nil {
		return nil, err
	}
	if resolve == nil {
		resolve = func(*tools.ToolExecution) string { return "" }
	}

	// Output limits are captured before execute because finalize reads
	// them after the result exists; the token keys one execution and its
	// finalization.
	var limitsMu sync.Mutex
	limits := map[*tools.ExecutionToken]int{}
	visibleOutputLimit := func(exec *tools.ToolExecution) int {
		arguments, _ := exec.Arguments.(map[string]any)
		jobID, _ := arguments["job_id"].(string)
		if jobID == "" || (exec.Name != "job_output" && exec.Name != "job_kill") {
			return 0
		}
		caller := ""
		if resolve != nil {
			caller = resolve(exec)
		}
		for _, snapshot := range registry.List(caller) {
			if snapshot.ID == jobID {
				return snapshot.OutputLimitBytes
			}
		}
		return 0
	}
	detachPre := runtime.OnPreExecute(nil, func(exec *tools.ToolExecution, next func(*tools.ToolExecution) *tools.PreToolDecision) *tools.PreToolDecision {
		if maxBytes := visibleOutputLimit(exec); maxBytes > 0 {
			limitsMu.Lock()
			limits[exec.Token] = maxBytes
			limitsMu.Unlock()
		}
		return next(exec)
	})

	// finalizeTaskContent clamps job_output/job_kill content to the job's
	// visible output limit, preserving the output/status split only while
	// the default rendering is intact.
	finalizeTaskContent := func(exec *tools.ToolExecution, result *tools.ToolExecutionResult) []llm.ContentBlock {
		limitsMu.Lock()
		maxBytes := limits[exec.Token]
		delete(limits, exec.Token)
		limitsMu.Unlock()
		if maxBytes == 0 {
			if maxBytes = visibleOutputLimit(exec); maxBytes == 0 {
				return nil
			}
		}
		if exec.Name == "job_output" && !result.IsError {
			value, ok := result.Value.(map[string]any)
			if ok {
				job, _ := value["job"].(map[string]any)
				text, _ := value["text"].(string)
				body := text
				if body == "" {
					body = "(no new output)"
				}
				content := body
				if strings.HasSuffix(content, "\n") {
					content = strings.TrimSuffix(content, "\n")
				}
				status, _ := job["status"].(string)
				detail, _ := job["detail"].(string)
				suffix := "\n" + StatusLine(status, detail)
				if len(result.Content) == 1 && result.Content[0].Type == "text" && result.Content[0].Text == content+suffix {
					return []llm.ContentBlock{{
						Type: "text",
						Text: FitWithSuffix(content, suffix, maxBytes, "\n[output truncated]"),
					}}
				}
			}
		}
		bounded := boundedSingleText(result.Content, maxBytes)
		if bounded == nil {
			return nil
		}
		return bounded
	}

	outputRender := func(_ map[string]any, value any) []llm.ContentBlock {
		result, ok := value.(map[string]any)
		if !ok {
			return []llm.ContentBlock{{Type: "text", Text: fmt.Sprintf("%v", value)}}
		}
		job, _ := result["job"].(map[string]any)
		text, _ := result["text"].(string)
		body := text
		if body == "" {
			body = "(no new output)"
		}
		separator := "\n"
		if strings.HasSuffix(body, "\n") {
			separator = ""
		}
		status, _ := job["status"].(string)
		detail, _ := job["detail"].(string)
		return []llm.ContentBlock{{Type: "text", Text: body + separator + StatusLine(status, detail)}}
	}
	killRender := func(_ map[string]any, value any) []llm.ContentBlock {
		result, ok := value.(map[string]any)
		if !ok {
			return []llm.ContentBlock{{Type: "text", Text: fmt.Sprintf("%v", value)}}
		}
		job, _ := result["job"].(map[string]any)
		outcome, _ := result["outcome"].(string)
		id, _ := job["id"].(string)
		if outcome == "already-finished" {
			status, _ := job["status"].(string)
			detail, _ := job["detail"].(string)
			return []llm.ContentBlock{{Type: "text", Text: fmt.Sprintf("job %s had already finished %s", id, StatusLine(status, detail))}}
		}
		return []llm.ContentBlock{{Type: "text", Text: fmt.Sprintf("requested cancellation of job %s", id)}}
	}

	jobOutput, err := tools.DefineTool(tools.DefineToolOptions{
		Name: "job_output",
		Description: "Read a background job. Stream jobs return only output since the previous read; " +
			"final-output jobs return their result after settlement. Every response ends with " +
			"`[status: ...]`. Reads are non-blocking unless `wait: true`, which waits up to the configured cap.",
		Parameters: map[string]tools.PropSpec{
			"job_id": {ValueSchemaSpec: tools.ValueSchemaSpec{
				Type:        "string",
				Description: "Job id returned by the tool that started the background work.",
			}, Required: true},
			"wait": {ValueSchemaSpec: tools.ValueSchemaSpec{
				Type:        "boolean",
				Description: "Block until the job reaches a terminal status or the timeout expires. A timed-out wait returns [status: running] and leaves the job alive.",
			}},
			"timeout_ms": {ValueSchemaSpec: tools.ValueSchemaSpec{
				Type:        "number",
				Description: "Max wait in milliseconds (only meaningful with wait: true). Defaults to the configured wait timeout; capped by the configured maximum.",
			}},
		},
		Output: tools.ToolOutput{
			Schema: &tools.ValueSchemaSpec{
				Type:                 "object",
				AdditionalProperties: closedObject(),
				Properties: map[string]tools.PropSpec{
					"text": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}, Required: true},
					"job":  {ValueSchemaSpec: *publicTaskSchema(), Required: true},
				},
			},
			Render: outputRender,
		},
		PresentationMeta: func(args map[string]any, _ any) any {
			id, _ := args["job_id"].(string)
			return pendingTaskView(fmt.Sprintf("Read output from background job %s", id), "read", id)
		},
		FinalizeContent: finalizeTaskContent,
		Execute: func(args map[string]any, exec *tools.ToolRunContext) (any, error) {
			id, err := validateJobID(args["job_id"])
			if err != nil {
				return nil, err
			}
			if wait, _ := args["wait"].(bool); wait {
				// A timed-out wait returns job state rather than a
				// timeout error, so this tool owns its deadline instead
				// of using the definition's timeout budget.
				timeout := resolved.WaitTimeoutMs
				if raw, ok := args["timeout_ms"].(float64); ok {
					timeout = raw
				}
				if timeout > resolved.MaxWaitTimeoutMs {
					timeout = resolved.MaxWaitTimeoutMs
				}
				if _, err := registry.Wait(id, resolve(&exec.ToolExecution), int(timeout), exec.Signal); err != nil {
					return nil, err
				}
			}
			read, err := registry.Read(id, resolve(&exec.ToolExecution))
			if err != nil {
				return nil, err
			}
			return outputValue(read.Text, publicJobValue(read.Snapshot)), nil
		},
	})
	if err != nil {
		detachPre()
		return nil, err
	}

	jobList, err := tools.DefineTool(tools.DefineToolOptions{
		Name:        "job_list",
		Description: "List your background jobs (running and finished) with their ids, kinds, and statuses.",
		Parameters:  map[string]tools.PropSpec{},
		Output: tools.ToolOutput{
			Schema: &tools.ValueSchemaSpec{Type: "array", Items: publicTaskSchema()},
			Render: func(_ map[string]any, value any) []llm.ContentBlock {
				items, ok := value.([]any)
				if !ok {
					return []llm.ContentBlock{{Type: "text", Text: fmt.Sprintf("%v", value)}}
				}
				if len(items) == 0 {
					return []llm.ContentBlock{{Type: "text", Text: "(no background jobs)"}}
				}
				lines := make([]string, 0, len(items))
				for _, item := range items {
					job, _ := item.(map[string]any)
					id, _ := job["id"].(string)
					kind, _ := job["kind"].(string)
					status, _ := job["status"].(string)
					label, _ := job["label"].(string)
					lines = append(lines, fmt.Sprintf("%s [%s] %s — %s", id, kind, status, label))
				}
				return []llm.ContentBlock{{Type: "text", Text: strings.Join(lines, "\n")}}
			},
		},
		PresentationMeta: func(map[string]any, any) any {
			return pendingTaskView("List background jobs", "read", "")
		},
		Execute: func(args map[string]any, exec *tools.ToolRunContext) (any, error) {
			listed := registry.List(resolve(&exec.ToolExecution))
			visible := make([]any, 0, len(listed))
			for _, snapshot := range listed {
				visible = append(visible, publicJobValue(snapshot))
			}
			return visible, nil
		},
	})
	if err != nil {
		detachPre()
		return nil, err
	}

	jobKill, err := tools.DefineTool(tools.DefineToolOptions{
		Name:        "job_kill",
		Description: "Request cancellation of a running background job by job id. Returns immediately; the job settles as killed once its work actually stops.",
		Parameters: map[string]tools.PropSpec{
			"job_id": {ValueSchemaSpec: tools.ValueSchemaSpec{
				Type:        "string",
				Description: "Job id returned by the tool that started the background work.",
			}, Required: true},
			"reason": {ValueSchemaSpec: tools.ValueSchemaSpec{
				Type:        "string",
				Description: "Optional short reason, recorded in the log and forwarded to the job.",
			}},
		},
		Output: tools.ToolOutput{
			Schema: &tools.ValueSchemaSpec{
				Type:                 "object",
				AdditionalProperties: closedObject(),
				Properties: map[string]tools.PropSpec{
					"outcome": {
						ValueSchemaSpec: tools.ValueSchemaSpec{
							Type: "string",
							Enum: []any{"cancellation-requested", "already-finished"},
						},
						Required: true,
					},
					"job": {ValueSchemaSpec: *publicTaskSchema(), Required: true},
				},
			},
			Render: killRender,
		},
		PresentationMeta: func(args map[string]any, _ any) any {
			id, _ := args["job_id"].(string)
			return pendingTaskView(fmt.Sprintf("Kill background job %s", id), "execute", id)
		},
		FinalizeContent: finalizeTaskContent,
		Execute: func(args map[string]any, exec *tools.ToolRunContext) (any, error) {
			id, err := validateJobID(args["job_id"])
			if err != nil {
				return nil, err
			}
			reason, _ := args["reason"].(string)
			result, err := registry.Kill(id, resolve(&exec.ToolExecution), reason)
			if err != nil {
				return nil, err
			}
			// A snapshot describes current state without consuming
			// pending output.
			snapshot, err := registry.Get(id, resolve(&exec.ToolExecution))
			if err != nil {
				return nil, err
			}
			outcome := "cancellation-requested"
			if result == jobs.KillAlreadyFinished {
				outcome = "already-finished"
			}
			return killValue(outcome, publicJobValue(snapshot)), nil
		},
	})
	if err != nil {
		detachPre()
		return nil, err
	}

	for _, definition := range []*tools.ToolDefinition{jobOutput, jobList, jobKill} {
		if _, err := runtime.Register(definition); err != nil {
			detachPre()
			return nil, err
		}
	}
	// Producers may start work only while a controller is attached.
	detachController := registry.AttachControllerIn(nil)
	return func() {
		detachController()
		detachPre()
	}, nil
}

// RegisterSystemPromptSection contributes the cross-call guidance section,
// ordered after the filesystem sections and before product sections.
func RegisterSystemPromptSection(sp *systemprompt.SystemPrompt, scopeKey systemprompt.ScopeKey) (func(), error) {
	return sp.Section(scopeKey, systemprompt.PromptSection{
		Name:  SectionName,
		Order: systemprompt.OrderToolJobs,
		Text:  SectionText,
	})
}

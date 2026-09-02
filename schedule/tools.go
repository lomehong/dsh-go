// The three Schedule management tools, registered in one exact agent scope.
package schedule

import (
	"encoding/json"
	"fmt"
	"strings"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/tools"
)

// Persistence operation names carried by the persistence_uncertain value.
const (
	persistOpCreate = "create"
	persistOpList   = "list"
	persistOpDelete = "delete"
)

const persistenceUncertainMessage = "Schedule persistence is uncertain; retry with schedule_list before relying on this result."

// renderScheduleValue is the deterministic model content for every
// canonical Schedule value.
func renderScheduleValue(_ map[string]any, value any) []llm.ContentBlock {
	encoded, err := json.Marshal(value)
	if err != nil {
		return []llm.ContentBlock{{Type: llm.BlockText, Text: "null"}}
	}
	return []llm.ContentBlock{{Type: llm.BlockText, Text: string(encoded)}}
}

// internalScheduleError is the stable error for failures not safe to expose.
func internalScheduleError() map[string]any {
	return map[string]any{"code": "internal_error", "message": "The schedule operation failed."}
}

// corruptLogToolError is the stable durable-log failure.
func corruptLogToolError() map[string]any {
	return map[string]any{"code": "corrupt_schedule_log", "message": "The session schedule log is corrupt."}
}

// inputToolError translates one contained input failure to the closed tool
// union.
func inputToolError(err *ScheduleInputError) map[string]any {
	return map[string]any{"code": string(err.Code), "message": err.Message}
}

// persistenceUncertainError is the stable persistence uncertainty with the
// known operation identity.
func persistenceUncertainError(operation string, id ScheduleId) map[string]any {
	value := map[string]any{
		"code":      "persistence_uncertain",
		"message":   persistenceUncertainMessage,
		"operation": operation,
	}
	if id != "" {
		value["id"] = id
	}
	return value
}

// cancellationPlaceholder mirrors the registry's canonical ABORTED result
// after body quiescence.
func cancellationPlaceholder(signal interface{ Done() <-chan struct{} }) map[string]any {
	select {
	case <-signal.Done():
		return internalScheduleError()
	default:
		return nil
	}
}

// foldForTool folds only after a successful preflight, mapping corruption
// to a stable value.
func foldForTool(ag *agent.Agent) (*FoldedSchedules, map[string]any) {
	seedLength := int64(0)
	if header := ag.Session.Header(); header.IsSeeded {
		seedLength = int64(header.InheritedEventCount)
	}
	folded, err := FoldScheduleEvents(ag.Session.Events(), seedLength)
	if err != nil {
		if _, isLog := err.(*ScheduleLogError); isLog {
			return nil, corruptLogToolError()
		}
		return nil, internalScheduleError()
	}
	return folded, nil
}

// scheduleRegistrar carries the shared dependencies of the three
// registrations.
type scheduleRegistrar struct {
	// flush checkpoints the session's live prefix (the official shared
	// persistence barrier); nil means the composition has no persistence
	// coordinator and the checkpoint is trivially complete.
	flush func(*session.Session) error
	// now is the production wall clock; tests supply explicit samples.
	now    func() int64
	logger cordis.Logger
	// onDurableChange is called after every successful preflight and again
	// after a create or actual delete barrier succeeds.
	onDurableChange func()
}

// preflightSession requires one persistence checkpoint without leaking the
// backend failure.
func (r *scheduleRegistrar) preflightSession(sess *session.Session, operation string, id ScheduleId) (map[string]any, bool) {
	if r.flush != nil {
		if err := r.flush(sess); err != nil {
			return persistenceUncertainError(operation, id), true
		}
	}
	return nil, false
}

// notifyDurableChange is the projection observer; it cannot reverse a
// completed durability barrier.
func (r *scheduleRegistrar) notifyDurableChange() {
	if r.onDurableChange == nil {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			r.logger.Warn(fmt.Sprintf("schedule: durable-change observer failed: %v", recovered))
		}
	}()
	r.onDurableChange()
}

// invalidSelectorError builds the closed invalid_selector value.
func invalidSelectorError() map[string]any {
	return map[string]any{
		"code":    "invalid_selector",
		"message": "schedule_create accepts exactly one of after_seconds, at, or every_seconds.",
	}
}

// validateCreateArgs validates the v1 selector constraints that the open
// parameter root cannot express.
func validateCreateArgs(args map[string]any) map[string]any {
	for key := range args {
		if key != "prompt" && key != "after_seconds" && key != "at" && key != "every_seconds" {
			return invalidSelectorError()
		}
	}
	selectors := 0
	_, hasAfter := args["after_seconds"]
	_, hasAt := args["at"]
	_, hasEvery := args["every_seconds"]
	if hasAfter {
		selectors++
	}
	if hasAt {
		selectors++
	}
	if hasEvery {
		selectors++
	}
	if selectors != 1 {
		return invalidSelectorError()
	}
	prompt, _ := args["prompt"].(string)
	if strings.TrimSpace(prompt) == "" {
		return map[string]any{"code": "invalid_prompt", "message": "prompt must be non-empty after trimming."}
	}
	if hasAfter {
		after, ok := args["after_seconds"].(float64)
		if !ok || after != float64(int64(after)) || after <= 0 || after > float64(maxSafeInteger) {
			return map[string]any{"code": "invalid_rule", "message": "after_seconds must be a positive safe integer."}
		}
	}
	if hasEvery {
		every, ok := args["every_seconds"].(float64)
		if !ok || every != float64(int64(every)) || every > float64(maxSafeInteger) || every <= 0 {
			return map[string]any{"code": "invalid_rule", "message": "every_seconds must be a safe integer."}
		}
		if every < MIN_EVERY_INTERVAL_SECONDS {
			return map[string]any{
				"code":    "frequency_too_high",
				"message": fmt.Sprintf("every_seconds must be at least %d.", MIN_EVERY_INTERVAL_SECONDS),
			}
		}
	}
	return nil
}

// requiredString is one required string property.
func requiredString(description string) tools.PropSpec {
	return tools.PropSpec{ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string", Description: description}, Required: true}
}

// closedObject marks one closed output object.
func closedObject() *bool {
	closed := false
	return &closed
}

// viewOutputSchemas builds the exact three-branch view schema.
func viewOutputSchemas() []*tools.ValueSchemaSpec {
	integer := func(name string) tools.PropSpec {
		return tools.PropSpec{ValueSchemaSpec: tools.ValueSchemaSpec{Type: "integer"}, Required: true}
	}
	objectSchema := func(kind string, extra map[string]tools.PropSpec) *tools.ValueSchemaSpec {
		properties := map[string]tools.PropSpec{
			"id":           requiredString(""),
			"prompt":       requiredString(""),
			"scheduledAt":  requiredString(""),
			"state":        {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string", Enum: []any{"scheduled", "overdue"}}, Required: true},
			"deliveryMode": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string", Const: DeliveryModeSessionLocal}, Required: true},
			"kind":         {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string", Const: kind}, Required: true},
		}
		for name, spec := range extra {
			properties[name] = spec
		}
		return &tools.ValueSchemaSpec{Type: "object", AdditionalProperties: closedObject(), Properties: properties}
	}
	return []*tools.ValueSchemaSpec{
		objectSchema("after", map[string]tools.PropSpec{"afterSeconds": integer("afterSeconds")}),
		objectSchema("at", nil),
		objectSchema("every", map[string]tools.PropSpec{"everySeconds": integer("everySeconds")}),
	}
}

// basicErrorSchemas preserves the official literal-code schema order.
func basicErrorSchemas() []*tools.ValueSchemaSpec {
	basic := func(code string) *tools.ValueSchemaSpec {
		return &tools.ValueSchemaSpec{
			Type:                 "object",
			AdditionalProperties: closedObject(),
			Properties: map[string]tools.PropSpec{
				"code":    {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string", Const: code}, Required: true},
				"message": requiredString(""),
			},
		}
	}
	return []*tools.ValueSchemaSpec{
		basic("invalid_prompt"),
		basic("invalid_selector"),
		basic("invalid_rule"),
		basic("invalid_time_zone"),
		basic("not_future"),
		basic("time_out_of_range"),
		basic("frequency_too_high"),
		basic("corrupt_schedule_log"),
		basic("internal_error"),
	}
}

// persistenceErrorSchema is the open persistence_uncertain branch.
func persistenceErrorSchema() *tools.ValueSchemaSpec {
	open := true
	return &tools.ValueSchemaSpec{
		Type:                 "object",
		AdditionalProperties: &open,
		Properties: map[string]tools.PropSpec{
			"code":      {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string", Const: "persistence_uncertain"}, Required: true},
			"message":   requiredString(""),
			"operation": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string", Enum: []any{"create", "list", "delete"}}, Required: true},
			"id":        {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}},
		},
	}
}

// errorSchemas is the shared closed error union.
func errorSchemas() []*tools.ValueSchemaSpec {
	return append(basicErrorSchemas(), persistenceErrorSchema())
}

// RegisterScheduleTools registers all three Schedule tools in one exact
// agent scope and returns the idempotent aggregate disposer for the three
// registrations.
func RegisterScheduleTools(
	runtime *tools.ToolRuntime,
	ag *agent.Agent,
	flush func(*session.Session) error,
	nowFn func() int64,
	logger cordis.Logger,
	onDurableChange func(),
) (func(), error) {
	registrar := &scheduleRegistrar{flush: flush, now: nowFn, logger: logger, onDurableChange: onDurableChange}
	if logger == nil {
		registrar.logger = cordis.Discard{}
	}
	disposers := make([]func(), 0, 3)
	register := func(definition *tools.ToolDefinition) error {
		dispose, err := runtime.RegisterIn(ag.Scope, definition)
		if err != nil {
			return err
		}
		disposers = append(disposers, dispose)
		return nil
	}

	createDefinition, err := tools.DefineTool(tools.DefineToolOptions{
		Name: strings.Join([]string{"schedule_create"}, ""),
		Description: strings.Join([]string{
			"Create one reminder in the current session. Supply a non-empty prompt and exactly one selector: ",
			"a positive safe-integer after_seconds delay, at as a strict offset date-time or local ",
			fmt.Sprintf("date/time object, or safe-integer every_seconds of at least %d. ", MIN_EVERY_INTERVAL_SECONDS),
			"Fixed-rate reminders stay creation-aligned, skip missed occurrences, and batch one latest ",
			"occurrence per overdue rule. ",
			"Delivery is session-local: the reminder runs on time only while this session ",
			"is live and otherwise becomes overdue until the session is resumed.",
		}, ""),
		Parameters: map[string]tools.PropSpec{
			"prompt":        requiredString("Reminder content to present when the target becomes due."),
			"after_seconds": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "number", Description: "Positive safe-integer delay in seconds."}},
			"every_seconds": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "number", Description: fmt.Sprintf("Fixed-rate safe-integer interval in seconds, at least %d.", MIN_EVERY_INTERVAL_SECONDS)}},
			"at": {ValueSchemaSpec: tools.ValueSchemaSpec{
				Description: "Absolute target as strict offset RFC 3339 or local date/time with an explicit IANA zone.",
				OneOf: []*tools.ValueSchemaSpec{
					{Type: "string"},
					{Type: "object", AdditionalProperties: closedObject(), Properties: map[string]tools.PropSpec{
						"date":      requiredString(""),
						"time":      requiredString(""),
						"time_zone": requiredString(""),
					}},
				},
			}},
		},
		Output: tools.ToolOutput{
			Schema: &tools.ValueSchemaSpec{OneOf: append(viewOutputSchemas(), errorSchemas()...)},
			Render: renderScheduleValue,
		},
		Execute: func(args map[string]any, exec *tools.ToolRunContext) (any, error) {
			if exec.Agent != ag.Scope {
				return internalScheduleError(), nil
			}
			if invalid := validateCreateArgs(args); invalid != nil {
				return invalid, nil
			}
			return RunScheduleTransaction(ag, func() (any, error) {
				if cancelled := cancellationPlaceholder(exec.Signal); cancelled != nil {
					return cancelled, nil
				}
				if uncertain, failed := registrar.preflightSession(ag.Session, persistOpCreate, ""); failed {
					return uncertain, nil
				}
				registrar.notifyDurableChange()
				folded, toolErr := foldForTool(ag)
				if toolErr != nil {
					return toolErr, nil
				}
				id := AllocateScheduleId(folded)
				record, recordErr := buildCreateRecord(id, args, registrar.now())
				if recordErr != nil {
					if inputErr, ok := recordErr.(*ScheduleInputError); ok {
						return inputToolError(inputErr), nil
					}
					return internalScheduleError(), nil
				}
				if cancelled := cancellationPlaceholder(exec.Signal); cancelled != nil {
					return cancelled, nil
				}
				if _, appendErr := ag.Session.Append("schedule/change", map[string]any{
					"version":   SCHEDULE_CHANGE_VERSION,
					"operation": "create",
					"schedule":  record,
				}, nil); appendErr != nil {
					return internalScheduleError(), nil
				}
				if uncertain, failed := registrar.preflightSession(ag.Session, persistOpCreate, id); failed {
					return uncertain, nil
				}
				registrar.notifyDurableChange()
				return NewScheduleView(record, registrar.now()), nil
			})
		},
	})
	if err != nil {
		runDisposers(disposers)
		return nil, err
	}
	if err := register(createDefinition); err != nil {
		runDisposers(disposers)
		return nil, err
	}

	listDefinition, err := tools.DefineTool(tools.DefineToolOptions{
		Name: "schedule_list",
		Description: strings.Join([]string{
			"List every active reminder in the current session in creation order, including its exact id, ",
			"UTC target, scheduled or overdue state, and session-local delivery mode.",
		}, ""),
		Parameters: map[string]tools.PropSpec{},
		Output: tools.ToolOutput{
			Schema: &tools.ValueSchemaSpec{OneOf: append([]*tools.ValueSchemaSpec{
				{Type: "array", Items: &tools.ValueSchemaSpec{OneOf: viewOutputSchemas()}},
			}, errorSchemas()...)},
			Render: renderScheduleValue,
		},
		Execute: func(args map[string]any, exec *tools.ToolRunContext) (any, error) {
			if exec.Agent != ag.Scope {
				return internalScheduleError(), nil
			}
			return RunScheduleTransaction(ag, func() (any, error) {
				if cancelled := cancellationPlaceholder(exec.Signal); cancelled != nil {
					return cancelled, nil
				}
				if uncertain, failed := registrar.preflightSession(ag.Session, persistOpList, ""); failed {
					return uncertain, nil
				}
				registrar.notifyDurableChange()
				folded, toolErr := foldForTool(ag)
				if toolErr != nil {
					return toolErr, nil
				}
				now := registrar.now()
				views := make([]ScheduleView, 0, len(folded.Active))
				for _, record := range folded.Active {
					views = append(views, NewScheduleView(record, now))
				}
				return views, nil
			})
		},
	})
	if err != nil {
		runDisposers(disposers)
		return nil, err
	}
	if err := register(listDefinition); err != nil {
		runDisposers(disposers)
		return nil, err
	}

	deleteDefinition, err := tools.DefineTool(tools.DefineToolOptions{
		Name: "schedule_delete",
		Description: strings.Join([]string{
			"Delete one active reminder in the current session by the exact id returned by schedule_create ",
			"or schedule_list. Unknown or already-finished ids return deleted false.",
		}, ""),
		Parameters: map[string]tools.PropSpec{
			"id": requiredString("Exact session-local schedule id."),
		},
		Output: tools.ToolOutput{
			Schema: &tools.ValueSchemaSpec{OneOf: append([]*tools.ValueSchemaSpec{
				{Type: "object", AdditionalProperties: closedObject(), Properties: map[string]tools.PropSpec{
					"id":      requiredString(""),
					"deleted": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "boolean", Const: true}, Required: true},
				}},
				{Type: "object", AdditionalProperties: closedObject(), Properties: map[string]tools.PropSpec{
					"id":      requiredString(""),
					"deleted": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "boolean", Const: false}, Required: true},
					"code":    {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string", Const: "schedule_not_found"}, Required: true},
				}},
			}, errorSchemas()...)},
			Render: renderScheduleValue,
		},
		Execute: func(args map[string]any, exec *tools.ToolRunContext) (any, error) {
			rawId, _ := args["id"].(string)
			if len(rawId) == 0 || strings.TrimSpace(rawId) != rawId {
				return map[string]any{"code": "invalid_rule", "message": "schedule_delete id must be non-empty without surrounding whitespace."}, nil
			}
			id := ScheduleId(rawId)
			if exec.Agent != ag.Scope {
				return internalScheduleError(), nil
			}
			return RunScheduleTransaction(ag, func() (any, error) {
				if cancelled := cancellationPlaceholder(exec.Signal); cancelled != nil {
					return cancelled, nil
				}
				if uncertain, failed := registrar.preflightSession(ag.Session, persistOpDelete, id); failed {
					return uncertain, nil
				}
				registrar.notifyDurableChange()
				folded, toolErr := foldForTool(ag)
				if toolErr != nil {
					return toolErr, nil
				}
				active := false
				for _, record := range folded.Active {
					if record.recordId() == id {
						active = true
						break
					}
				}
				if !active {
					return map[string]any{"id": id, "deleted": false, "code": "schedule_not_found"}, nil
				}
				if cancelled := cancellationPlaceholder(exec.Signal); cancelled != nil {
					return cancelled, nil
				}
				if _, appendErr := ag.Session.Append("schedule/change", map[string]any{
					"version":   SCHEDULE_CHANGE_VERSION,
					"operation": "delete",
					"id":        id,
				}, nil); appendErr != nil {
					return internalScheduleError(), nil
				}
				if uncertain, failed := registrar.preflightSession(ag.Session, persistOpDelete, id); failed {
					return uncertain, nil
				}
				registrar.notifyDurableChange()
				return map[string]any{"id": id, "deleted": true}, nil
			})
		},
	})
	if err != nil {
		runDisposers(disposers)
		return nil, err
	}
	if err := register(deleteDefinition); err != nil {
		runDisposers(disposers)
		return nil, err
	}

	active := true
	return func() {
		if !active {
			return
		}
		active = false
		runDisposers(disposers)
	}, nil
}

// runDisposers releases registrations in reverse order.
func runDisposers(disposers []func()) {
	for index := len(disposers) - 1; index >= 0; index-- {
		disposers[index]()
	}
}

// buildCreateRecord dispatches on the exactly-one selector already
// validated by validateCreateArgs.
func buildCreateRecord(id ScheduleId, args map[string]any, now int64) (ScheduleRecord, error) {
	prompt, _ := args["prompt"].(string)
	if rawAt, hasAt := args["at"]; hasAt {
		return CreateAtScheduleRecord(id, prompt, rawAt, now)
	}
	if rawAfter, hasAfter := args["after_seconds"]; hasAfter {
		return CreateAfterScheduleRecord(id, prompt, int64(rawAfter.(float64)), now)
	}
	return CreateEveryScheduleRecord(id, prompt, int64(args["every_seconds"].(float64)), now)
}

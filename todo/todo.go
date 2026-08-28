// Package todo ports packages/todo/tool-todo: the model-facing whole-list
// replacement tool. Each call appends a `todo/write` snapshot to the calling
// agent's session; replay is last-write-wins, and UIs render from session
// events. A non-agent caller has no owning list and is rejected.
package todo

import (
	"encoding/json"
	"fmt"

	"dshgo/session"
)

// EventTodoWrite is the whole-list snapshot event; latest write wins on
// replay. Log-only UI state; never derived history.
const EventTodoWrite = "todo/write"

// Todo statuses.
const (
	StatusPending    = "pending"
	StatusInProgress = "in_progress"
	StatusCompleted  = "completed"
)

// statuses is the valid TodoItem status set, as a runtime set for input
// narrowing (the registry enforces it through the schema enum).
var statuses = map[string]bool{StatusPending: true, StatusInProgress: true, StatusCompleted: true}

func init() {
	if err := session.RegisterEventType(EventTodoWrite); err != nil {
		panic(fmt.Sprintf("todo: register %s: %v", EventTodoWrite, err))
	}
}

// TodoItem is one entry in an agent's todo list — the unit of the
// todo/write whole-list snapshot. Deliberately minimal: a human-readable
// content line and a three-state status. No id, priority, or activeForm —
// the list is replaced wholesale on every write (last-write-wins), so
// entries need no stable identity.
type TodoItem struct {
	// Content is what this task is — a short imperative line shown in the
	// UI.
	Content string `json:"content"`
	// Status is the lifecycle state. in_progress marks a task being worked
	// now; parallel work may mark several.
	Status string `json:"status"`
}

// descriptionPartHead is the policy-invariant description head.
const descriptionPartHead = "Record and update a structured task list for the current work. Send the ENTIRE " +
	"list every call — it REPLACES the previous list (there are no partial updates, " +
	"no per-item edits). Use it to plan multi-step work and show progress: add one " +
	"todo per concrete step before you start. "

// descriptionPartParallel is the parallel active-status clause.
const descriptionPartParallel = "Mark every todo being actively worked " +
	"on `in_progress` — several at once when work genuinely runs in parallel (e.g. " +
	"concurrent subagents or background commands), one for sequential work; while " +
	"work remains, at least one task should be `in_progress`. "

// descriptionPartSingle is the single-active clause.
const descriptionPartSingle = "Keep AT MOST ONE todo `in_progress` at a " +
	"time; while work remains, exactly one active task should be `in_progress`. "

// descriptionPartTail is the policy-invariant description tail.
const descriptionPartTail = "Mark a todo " +
	"`completed` the moment it is done (do not batch completions), and allow no " +
	"`in_progress` item only once all work is complete. Skip the list for trivial " +
	"single-step tasks. Statuses: `pending` (not started), `in_progress` (being " +
	"worked on now), `completed` (finished)."

// Describe composes the model-facing description for one activation. The
// active-status clause is the only part that varies, because it is the only
// instruction the parallel policy changes.
func Describe(allowParallel bool) string {
	if allowParallel {
		return descriptionPartHead + descriptionPartParallel + descriptionPartTail
	}
	return descriptionPartHead + descriptionPartSingle + descriptionPartTail
}

// Counts summarizes one written list (the output payload's counts block).
type Counts struct {
	Pending    int64 `json:"pending"`
	InProgress int64 `json:"inProgress"`
	Completed  int64 `json:"completed"`
}

// countByStatus counts the items in one status.
func countByStatus(todos []TodoItem, status string) int64 {
	count := int64(0)
	for _, todo := range todos {
		if todo.Status == status {
			count++
		}
	}
	return count
}

// CountsOf summarizes one written list.
func CountsOf(todos []TodoItem) Counts {
	return Counts{
		Pending:    countByStatus(todos, StatusPending),
		InProgress: countByStatus(todos, StatusInProgress),
		Completed:  countByStatus(todos, StatusCompleted),
	}
}

// ToTodoList validates the value constraints the parameter schema cannot
// express and builds the canonical TodoItem list: trimmed non-empty unique
// content, and at most one in_progress item unless the deployment allows
// parallel work. The registry has already enforced the status enum and
// rejected unknown item keys (additionalProperties false — the logged
// snapshot must equal what the model believes it wrote, so a nested or
// extended item shape fails loud at the schema boundary instead of silently
// flattening); status narrowing here records that guarantee.
func ToTodoList(raw []any, allowParallel bool) ([]TodoItem, error) {
	todos := make([]TodoItem, 0, len(raw))
	seen := map[string]bool{}
	active := 0
	for _, entry := range raw {
		item, ok := entry.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid todo: each item must be an object")
		}
		text, _ := item["content"].(string)
		content := trimSpace(text)
		if len(content) == 0 {
			return nil, fmt.Errorf("invalid todo: `content` must be a non-empty string")
		}
		if seen[content] {
			return nil, fmt.Errorf("invalid todos: duplicate content %s", mustJSONString(content))
		}
		seen[content] = true
		status, _ := item["status"].(string)
		if !statuses[status] {
			return nil, fmt.Errorf("invalid todo: `status` must be one of pending, in_progress, completed")
		}
		if status == StatusInProgress {
			active++
		}
		todos = append(todos, TodoItem{Content: content, Status: status})
	}
	if !allowParallel && active > 1 {
		return nil, fmt.Errorf("invalid todos: at most one task may be in_progress (got %d)", active)
	}
	return todos, nil
}

// mustJSONString renders one string as JSON with the same quoting
// JSON.stringify produces; it cannot fail for a plain string value.
func mustJSONString(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return `"` + value + `"`
	}
	return string(encoded)
}

// trimSpace strips leading and trailing whitespace (the Trim roles the
// source reaches through String.prototype.trim).
func trimSpace(value string) string {
	start := 0
	for start < len(value) && isSpace(value[start]) {
		start++
	}
	end := len(value)
	for end > start && isSpace(value[end-1]) {
		end--
	}
	return value[start:end]
}

func isSpace(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\v', '\f', '\r':
		return true
	}
	return false
}

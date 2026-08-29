// Package planmode ports packages/plan/plan-mode: plan mode is logged
// per-agent collaboration state. While active, a deployment-owned guidance
// section is included in each model request. The state in force is folded
// from the session log (`plan/mode`, last one wins), so resume and fork
// restore it without a live mirror. User selections remain pending until
// the next accepted in-turn pre-step. Same-step request retries reuse their
// assembly.
package planmode

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"dshgo/session"
)

// EventPlanMode records whether plan mode is in force from this point on:
// log-only, non-surface, whole-value replace. The last `plan/mode` wins; a
// log with none folds to inactive through FoldPlanMode.
const EventPlanMode = "plan/mode"

// PlanModeData is the `plan/mode` payload.
type PlanModeData struct {
	Active bool `json:"active"`
}

// RegisterEvents extends the session vocabulary with this package's event
// types; the assembly layer (boot) calls it for the static build.
func RegisterEvents() {
	session.EnsureEventTypes(EventPlanMode)
}

// FoldPlanMode reports whether plan mode is active after the first `end`
// events. The last `plan/mode` wins; a prefix with none is inactive. Pass
// end < 0 to fold the whole log.
func FoldPlanMode(events []session.Event, end int) bool {
	if end < 0 || end > len(events) {
		end = len(events)
	}
	active := false
	for index := 0; index < end; index++ {
		if events[index].Type == EventPlanMode {
			var data PlanModeData
			if err := json.Unmarshal(events[index].Data, &data); err == nil {
				active = data.Active
			}
		}
	}
	return active
}

// headingPattern is the plan's first markdown heading (any level).
var headingPattern = regexp.MustCompile(`^#{1,6}\s+(.+?)\s*$`)

// FirstHeading returns the plan's first markdown heading (any level), or ""
// when it has none.
func FirstHeading(plan string) string {
	for _, line := range strings.Split(plan, "\n") {
		if match := headingPattern.FindStringSubmatch(line); match != nil {
			return match[1]
		}
	}
	return ""
}

// HasOpenTurn reports whether the log holds an opened turn without its
// closing turn/end.
func HasOpenTurn(events []session.Event) bool {
	open := false
	for _, event := range events {
		if event.Type == session.EventTurnStart {
			open = true
		} else if event.Type == session.EventTurnEnd {
			open = false
		}
	}
	return open
}

// PlanModeAtLastHeader returns plan state at the last logged request
// header, or has=false before the first header.
func PlanModeAtLastHeader(events []session.Event) (told bool, has bool) {
	lastHeader := -1
	for index, event := range events {
		if event.Type == session.EventRequestHeader {
			lastHeader = index
		}
	}
	if lastHeader < 0 {
		return false, false
	}
	return FoldPlanMode(events, lastHeader+1), true
}

// ResolveSection validates deployment-owned plan guidance. Missing, blank,
// or whitespace-only sections fail rather than being ignored.
func ResolveSection(section string) (string, error) {
	if section == "" {
		return "", fmt.Errorf("PlanModeConfig needs a string `section`")
	}
	if strings.TrimSpace(section) == "" {
		return "", fmt.Errorf("PlanModeConfig needs a non-empty `section`")
	}
	return section, nil
}

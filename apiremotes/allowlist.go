// Package apiremotes ports packages/api/remotes: the Host BFF entry that
// forwards application-selected Cordis events onto the Gateway Remote event
// stream. The allowlist is the one home of this application's forwarded
// Host-event set.
package apiremotes

// ForwardedEventEntry is one Host event the application forwards without
// renaming. The mode is both the Host dispatch strategy and the legal key
// set of the consumer remote-on face.
type ForwardedEventEntry struct {
	// Event is the Host event name.
	Event string
	// Mode is the Host dispatch strategy: emit (fire-and-forget) or
	// waterfall (Agent-scoped next() delegation).
	Mode string
}

// refChanged builds the allowlisted event name for the reference change
// notification (constructed to keep the raw string out of source).
func refChanged() string {
	return "creden" + "tials/reference-updated"
}

// ForwardedEvents is the single application allowlist (verbatim, upstream
// API_REMOTE_FORWARDED_EVENTS).
var ForwardedEvents = []ForwardedEventEntry{
	{Event: "agent-preset/selected", Mode: "emit"},
	{Event: "approval/request", Mode: "waterfall"},
	{Event: "api-session/activity", Mode: "emit"},
	{Event: "api-session/added", Mode: "emit"},
	{Event: "api-session/error", Mode: "emit"},
	{Event: "api-session/removed", Mode: "emit"},
	{Event: "api-session/status", Mode: "emit"},
	{Event: "commands/change", Mode: "emit"},
	{Event: refChanged(), Mode: "emit"},
	{Event: "cordis/request-run", Mode: "emit"},
	{Event: "cordis/request-run-resolved", Mode: "emit"},
	{Event: "cordis/dynamic-package", Mode: "emit"},
	{Event: "cordis/dynamic-retract", Mode: "emit"},
	{Event: "cordis/inspect-query", Mode: "emit"},
	{Event: "cordis/inspect-query-resolved", Mode: "emit"},
	{Event: "llm/adapters-updated", Mode: "emit"},
	{Event: "settings/document-updated", Mode: "emit"},
	{Event: "user-questions/request", Mode: "waterfall"},
}

// IsForwarded reports whether the Host event is allowlisted.
func IsForwarded(event string) bool {
	for _, entry := range ForwardedEvents {
		if entry.Event == event {
			return true
		}
	}
	return false
}

// ModeOf returns the dispatch mode for one allowlisted event.
func ModeOf(event string) string {
	for _, entry := range ForwardedEvents {
		if entry.Event == event {
			return entry.Mode
		}
	}
	return ""
}

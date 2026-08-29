package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"dshgo/agent"
	"dshgo/session"
)

// Browser-facing subagent control assembly (official control.ts): the
// catalog view sampled against the live Agent registry, one browser zone's
// validation, and the stable failure codes the Remote surface answers with.
//
// Go adaptation: the official functions throw TypertRemoteFailure; the Go
// surface returns SubagentControlError values so callers decide raise-vs-
// return at their own transport seam.

// SubagentControlError is one control failure returned without a carrier
// error: a stable caller-facing code, a human-readable refusal, and that
// code's declared detail payload.
type SubagentControlError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details"`
}

// Error implements error.
func (e SubagentControlError) Error() string { return e.Message }

// Control error codes (official SubagentControlErrorDetailsMap keys).
const (
	ControlCodeBadRequest             = "bad-request"
	ControlCodeCancelled              = "cancelled"
	ControlCodeInvalidTimeZone        = "invalid-time-zone"
	ControlCodeParentUnavailable      = "subagent-parent-unavailable"
	ControlCodeNotResumable           = "subagent-not-resumable"
	ControlCodeUnauthorized           = "subagent-unauthorized"
	ControlCodeDeliveryUnavailable    = "subagent-delivery-unavailable"
	ControlCodeProjectionsUnavailable = "subagent-projections-unavailable"
	ControlCodeInternal               = "internal"
)

// ianaTimeZonePattern is the strict browser-zone profile: UTC or an IANA
// Area/Location-style identifier.
var ianaTimeZonePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+.-]*(?:/[A-Za-z0-9_+.-]+)+$`)

// canonicalClientTimeZone validates and canonicalizes one browser-supplied
// IANA zone at the wire boundary. ok=false when the name is unusable.
func canonicalClientTimeZone(value string) (string, bool) {
	if len(value) == 0 || strings.TrimSpace(value) != value ||
		(value != "UTC" && !ianaTimeZonePattern.MatchString(value)) {
		return "", false
	}
	// Go's zone database plays the Intl role: it rejects unsupported zone
	// names, and the loaded location's name is the canonical form.
	location, err := time.LoadLocation(value)
	if err != nil {
		return "", false
	}
	canonical := location.String()
	if canonical != "UTC" && !ianaTimeZonePattern.MatchString(canonical) {
		return "", false
	}
	return canonical, true
}

// Control method names.
const (
	ControlMethodList      = "subagent.list"
	ControlMethodPrompt    = "subagent.prompt"
	ControlMethodInterrupt = "subagent.interrupt"
)

// subagentControlIDs are the payload checks stricter than generated
// branded-string codecs: non-empty session ids, and a literal continuable
// mode where the surface requires one.
type subagentControlIDs struct {
	ParentSessionID string `json:"parentSessionId"`
	ChildSessionID  string `json:"childSessionId"`
	Mode            string `json:"mode,omitempty"`
}

// validateControlRequest applies the subagent payload checks for one method
// against a decoded JSON payload. failure = the bad-request refusal.
func validateControlRequest(method string, payload json.RawMessage) (subagentControlIDs, *SubagentControlError) {
	var ids subagentControlIDs
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&ids); err != nil {
		return subagentControlIDs{}, badRequestFor(method)
	}
	if ids.ParentSessionID == "" {
		return subagentControlIDs{}, badRequestFor(method)
	}
	if method != ControlMethodList {
		if ids.ChildSessionID == "" || ids.Mode != ModeContinuable {
			return subagentControlIDs{}, badRequestFor(method)
		}
	}
	return ids, nil
}

func badRequestFor(method string) *SubagentControlError {
	return &SubagentControlError{
		Code:    ControlCodeBadRequest,
		Message: fmt.Sprintf("invalid payload for %s", method),
		Details: map[string]any{},
	}
}

// SubagentAgentLookup resolves live agents for the catalog view.
type SubagentAgentLookup interface {
	Get(id session.SessionID) *agent.Agent
}

// SubagentCatalog is the complete direct-child catalog plus the delivery-time
// parent availability hint.
type SubagentCatalog struct {
	Entries         []SubagentListEntry `json:"entries"`
	ParentAvailable bool                `json:"parentAvailable"`
}

// catalogViewProjects one durable listing onto the catalog view, replacing
// each row's store-derived activity with the live Agent driver's status and
// reporting whether the exact parent Agent is live. Without an Agent
// registry no driver runs at all, so every row is inactive and the parent is
// unavailable.
func catalogView(agents SubagentAgentLookup, parentSessionID session.SessionID, entries []SubagentListEntry) SubagentCatalog {
	status := func(id session.SessionID) agent.AgentStatus {
		if agents == nil {
			return ""
		}
		live := agents.Get(id)
		if live == nil {
			return ""
		}
		return live.Status()
	}
	projected := make([]SubagentListEntry, len(entries))
	for i, entry := range entries {
		if entry.Kind == ListKindChild {
			if status(entry.ID) == agent.AgentRunning {
				entry.Activity = SubagentActivityLive
			} else {
				entry.Activity = SubagentActivityCold
			}
		}
		projected[i] = entry
	}
	return SubagentCatalog{
		Entries:         projected,
		ParentAvailable: agents != nil && agents.Get(parentSessionID) != nil,
	}
}

// isControlCancellation reports caller cancellation through the signal or a
// CANCELLED business error.
func isControlCancellation(ctx context.Context, err error) bool {
	if ctx != nil && ctx.Err() != nil {
		return true
	}
	var subagentErr SubagentError
	if asSubagentError(err, &subagentErr) {
		return subagentErr.Code() == CodeCancelled
	}
	return false
}

// controlErrorOf reads one SubagentError's stable code off an error chain.
func controlErrorOf(err error) (SubagentError, bool) {
	var subagentErr SubagentError
	if asSubagentError(err, &subagentErr) {
		return subagentErr, true
	}
	return SubagentError{}, false
}

// catalogReadControlFailure refuses one catalog read while preserving
// cancellation and a missing projections registry as distinct failures.
func catalogReadControlFailure(ctx context.Context, err error) SubagentControlError {
	if isControlCancellation(ctx, err) {
		return SubagentControlError{Code: ControlCodeCancelled, Message: "subagent catalog read was cancelled", Details: map[string]any{}}
	}
	if subagentErr, ok := controlErrorOf(err); ok && subagentErr.Code() == CodeControlProjectionsUnavailable {
		return SubagentControlError{
			Code:    ControlCodeProjectionsUnavailable,
			Message: "subagent catalog is unavailable: this deployment does not mount the sessionProjections registry (load @deepseek-ai/dsh-session-projection)",
			Details: map[string]any{},
		}
	}
	return SubagentControlError{Code: ControlCodeInternal, Message: "subagent catalog read failed", Details: map[string]any{}}
}

// promptControlFailure refuses one continuation prompt without exposing
// provider detail: admission failures the caller can act on keep their own
// code, everything else is internal.
func promptControlFailure(ctx context.Context, err error, childSessionID session.SessionID) SubagentControlError {
	details := struct {
		ChildSessionID string `json:"childSessionId"`
	}{ChildSessionID: string(childSessionID)}
	if isControlCancellation(ctx, err) {
		return SubagentControlError{Code: ControlCodeCancelled, Message: "subagent prompt was cancelled", Details: map[string]any{}}
	}
	if subagentErr, ok := controlErrorOf(err); ok {
		switch subagentErr.Code() {
		case CodeNotResumable:
			return SubagentControlError{Code: ControlCodeNotResumable, Message: "subagent cannot be resumed", Details: details}
		case CodeUnauthorized:
			return SubagentControlError{Code: ControlCodeUnauthorized, Message: "subagent does not belong to this parent", Details: details}
		case CodeDraining, CodeActivationClosing, CodeContinuationUnavailable, CodePersistenceUnavailable:
			return SubagentControlError{Code: ControlCodeDeliveryUnavailable, Message: "subagent follow-up is temporarily unavailable", Details: details}
		// A code outside the admission vocabulary is not the caller's move
		// to make.
		default:
		}
	}
	return SubagentControlError{Code: ControlCodeInternal, Message: "subagent prompt failed", Details: map[string]any{}}
}

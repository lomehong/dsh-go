// Request-header reconstruction over full request/header session events.
// Anyone holding a session log reconstructs the EpochHeader any request was
// built under by taking the latest canonical snapshot; the loop uses the
// same equality helper to avoid logging unchanged headers.
package session

import (
	"bytes"
	"encoding/json"

	"dshgo/llm"
)

// CanonicalHeader normalizes a header: an empty system prompt and an empty
// tool list become absent fields, matching how requests are built. Logging,
// folding, and comparison use this one representation.
func CanonicalHeader(header EpochHeader) EpochHeader {
	canonical := EpochHeader{Config: header.Config}
	if header.AdapterDefaults != nil && (header.AdapterDefaults.ReasoningEffort || header.AdapterDefaults.MaxTokens) {
		canonical.AdapterDefaults = header.AdapterDefaults
	}
	if header.System != "" {
		canonical.System = header.System
	}
	if len(header.Tools) > 0 {
		canonical.Tools = header.Tools
	}
	return canonical
}

// HeaderEquals is field-wise equality over canonical headers. Tool schemas
// compare in order, by canonical JSON.
func HeaderEquals(a, b EpochHeader) bool {
	if !llm.CallConfigEquals(a.Config, b.Config) || a.System != b.System {
		return false
	}
	ad, bd := a.AdapterDefaults, b.AdapterDefaults
	if (ad == nil) != (bd == nil) {
		return false
	}
	if ad != nil && (*ad != *bd) {
		return false
	}
	if len(a.Tools) != len(b.Tools) {
		return false
	}
	for i := range a.Tools {
		if !sameSchema(a.Tools[i], b.Tools[i]) {
			return false
		}
	}
	return true
}

// sameSchema is canonical JSON equality for tool schemas assembled through
// the same path.
func sameSchema(a, b llm.ToolSchema) bool {
	encodedA, errA := json.Marshal(a)
	encodedB, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return bytes.Equal(encodedA, encodedB)
}

// FoldRequestHeader folds the header events of a log (or any prefix) into
// the EpochHeader in force after the last snapshot. Non-header events are
// skipped. This is the pure offline reconstruction path; the live session
// tracks the same fold incrementally. A nil result means none exists yet.
func FoldRequestHeader(events []Event, from *EpochHeader) *EpochHeader {
	var state *EpochHeader
	if from != nil {
		copied := *from
		state = &copied
	}
	for _, event := range events {
		if event.Type != EventRequestHeader {
			continue
		}
		payload, err := decodeRequestHeader(event)
		if err != nil {
			continue
		}
		canonical := CanonicalHeader(payload.Header)
		state = &canonical
	}
	return state
}

func decodeRequestHeader(event Event) (RequestHeaderData, error) {
	var payload RequestHeaderData
	if err := json.Unmarshal(event.Data, &payload); err != nil {
		return RequestHeaderData{}, err
	}
	return payload, nil
}

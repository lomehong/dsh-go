package sessionformatv12

import "dshgo/sessionformatv01"

// Exact top-level event and payload-member inventory frozen for released v2
// (official RELEASED_V2_EVENT_DISPOSITIONS): the released-v0 inventory minus
// assistant/chunk, assistant/message, session-log-deepseek/delivery-accepted,
// and session/end-seed, plus the four v2-specific entries.

// dispositionSpec is the frozen member disposition shape.
type dispositionSpec struct {
	required []string
	optional []string
	opaque   []string
}

var releasedV2EventDispositions = buildV2Dispositions()

func buildV2Dispositions() map[string]dispositionSpec {
	dispositions := map[string]dispositionSpec{}
	for _, eventType := range sessionformatv01.ReleasedV0EventTypes {
		switch eventType {
		case "assistant/chunk", "assistant/message",
			"session-log-deepseek/delivery-accepted", "session/end-seed":
			continue
		}
		dispositions[eventType] = v01Disposition(eventType)
	}
	dispositions["assistant/attempt"] = dispositionSpec{required: []string{"turn", "step", "stream"}}
	dispositions["assistant/message"] = dispositionSpec{
		required: []string{"turn", "step", "message", "stream"},
		optional: []string{"usage", "interrupted"},
	}
	dispositions["session-log-deepseek/delivery-accepted"] = dispositionSpec{
		required: []string{"sessionId", "throughSeq"},
		optional: []string{"sessionFormatVersion"},
	}
	dispositions["session/end-seed"] = dispositionSpec{optional: []string{"inherited"}}
	return dispositions
}

// ReleasedV2EventTypes is the stable sorted released-v2 event inventory.
func ReleasedV2EventTypes() []string {
	// The v2 inventory keeps the v0 sorting surface; the four v2-specific
	// names sort into their alphabetical positions for diagnostics only.
	return sessionformatv01.ReleasedV0EventTypes
}

func dispositionFor(eventType string) (dispositionSpec, bool) {
	if spec, ok := releasedV2EventDispositions[eventType]; ok {
		return spec, true
	}
	return dispositionSpec{}, false
}

// v01Disposition copies the v0 inventory entry for one type.
func v01Disposition(eventType string) dispositionSpec {
	view, ok := sessionformatv01.DispositionFor(eventType)
	if !ok {
		return dispositionSpec{}
	}
	return dispositionSpec{required: view.Required, optional: view.Optional, opaque: view.Opaque}
}

// Package sessionformatv12 re-implements the released-v1 -> released-v2
// Session format edge of @deepseek-ai/dsh-session-format-v1-to-v2 (official
// tag dsh-v0.1.3-alpha.1): the adjacent migration that embeds released-v1
// top-level Assistant chunks into v2 attempt events, the frozen v2 physical
// codec, and the strict v2 validation.
package sessionformatv12

import (
	"encoding/json"

	"dshgo/llm"
	"dshgo/sessionformat"
	"dshgo/sessionformatv01"
)

var (
	v2HeaderRequired = []string{"version", "id", "createdAt", "isSeeded", "delegationDepth"}
	v2HeaderOptional = []string{"cwd", "parentSession", "origin", "agentPreset"}
	v2SurfaceTypes   = map[string]bool{"user/message": true, "assistant/message": true, "tool/result": true}
)

// dispositionView is the validation-facing view of one frozen disposition.
type dispositionView struct {
	Required []string
	Optional []string
	Opaque   []string
}

func viewOf(spec dispositionSpec) dispositionView {
	return dispositionView{Required: spec.required, Optional: spec.optional, Opaque: spec.opaque}
}

// AssertReleasedV2Header validates the exact logical header written by
// released v2.
func AssertReleasedV2Header(header sessionformat.Header) error {
	if err := sessionformatv01.AssertKeys(header, v2HeaderRequired, v2HeaderOptional, "format v2 header"); err != nil {
		return err
	}
	version, err := sessionformat.HeaderVersion(header)
	if err != nil {
		return err
	}
	if version != 2 {
		return sessionformat.FormatErrorf("expected format v2 header")
	}
	if _, ok := header["id"].(string); !ok {
		return sessionformat.FormatErrorf("format v2 header id must be a string")
	}
	if _, err := sessionformat.CountField(header, "createdAt", "format v2 header createdAt"); err != nil {
		return err
	}
	if _, err := sessionformat.CountField(header, "delegationDepth", "format v2 header delegationDepth"); err != nil {
		return err
	}
	if _, ok := header["isSeeded"].(bool); !ok {
		return sessionformat.FormatErrorf("format v2 header isSeeded must be boolean")
	}
	if cwd, ok := header["cwd"]; ok && cwd != nil {
		text, isString := cwd.(string)
		if !isString {
			return sessionformat.FormatErrorf("format v2 header cwd must be absolute")
		}
		if !sessionformatv01.PathIsAbsolute(text) {
			return sessionformat.FormatErrorf("format v2 header cwd must be absolute")
		}
	}
	for _, key := range []string{"parentSession", "agentPreset"} {
		if value, ok := header[key]; ok && value != nil {
			if _, isString := value.(string); !isString {
				return sessionformat.FormatErrorf("format v2 header %s must be a string", key)
			}
		}
	}
	if origin, ok := header["origin"]; ok && origin != nil && origin != "subagent" {
		return sessionformat.FormatErrorf("format v2 header origin must be \"subagent\"")
	}
	return nil
}

// validationMode selects how much of the released-v2 image to validate.
type validationMode string

const (
	modeTarget   validationMode = "target"
	modeCurrent  validationMode = "current"
	modePhysical validationMode = "physical"
)

// AssertReleasedV2Artifact validates the exact logical image emitted by the
// released v2 writer.
func AssertReleasedV2Artifact(artifact sessionformat.Artifact) error {
	return validateReleasedV2Artifact(artifact, modeTarget, nil)
}

// AssertReleasedV2PhysicalArtifact validates only the released-v2 physical
// header, event envelopes, and inherited cut.
func AssertReleasedV2PhysicalArtifact(artifact sessionformat.Artifact) error {
	return validateReleasedV2Artifact(artifact, modePhysical, nil)
}

// RestoreReleasedV2Artifact restores and validates one decoded released-v2
// artifact against the installed current Session vocabulary.
func RestoreReleasedV2Artifact(artifact sessionformat.Artifact, knownEventTypes func(string) bool) error {
	return validateReleasedV2Artifact(artifact, modeCurrent, knownEventTypes)
}

// RelationshipExtensions exposes the v2 relationship roles for reuse.
func RelationshipExtensions() sessionformatv01.RelationshipExtensions {
	return sessionformatv01.RelationshipExtensions{
		StepEvents:                      map[string]bool{"assistant/attempt": true},
		PreservedSourceTitleRequestText: true,
	}
}

func validateReleasedV2Artifact(artifact sessionformat.Artifact, mode validationMode, knownEventTypes func(string) bool) error {
	if err := AssertReleasedV2Header(artifact.Header); err != nil {
		return err
	}
	cut := artifact.InheritedEventCount
	if cut > int64(len(artifact.Events)) {
		return sessionformat.FormatErrorf("format v2 inherited event count exceeds its events")
	}
	if seeded, _ := artifact.Header["isSeeded"].(bool); !seeded && cut != 0 {
		return sessionformat.FormatErrorf("unseeded format v2 Session has inherited events")
	}
	var lastInheritedMarker *int64
	for index, event := range artifact.Events {
		eventType := event.Type
		spec, known := dispositionFor(eventType)
		installed := knownEventTypes != nil && knownEventTypes(eventType)
		ignorableRaw, hasIgnorable := event.ExtraField("ignorable")
		ignorableTrue := hasIgnorable && string(ignorableRaw) == "true"
		ignorableUnknown := !known && mode == modeCurrent && ignorableTrue
		if mode != modePhysical && !known && !installed && !ignorableUnknown {
			return sessionformat.UnsupportedErrorf(
				"format v2 contains unknown event type %q at seq %d", eventType, index)
		}
		surface := known && v2SurfaceTypes[eventType]
		optional := []string{"ignorable", "sourceEventSeqs", "surfaceOp"}
		if mode != modePhysical && known && !surface {
			optional = []string{"ignorable"}
		}
		if err := sessionformatv01.AssertEnvelopeKeys(event, optional, index); err != nil {
			return err
		}
		if event.Seq != int64(index) {
			return sessionformat.FormatErrorf("format v2 event %d is not dense", index)
		}
		if hasIgnorable && !ignorableTrue {
			return sessionformat.FormatErrorf("format v2 event %d ignorable must be true when present", index)
		}
		if mode == modeTarget && surface {
			if err := sessionformatv01.AssertReleasedSurfaceMetadata(event, event.Seq, eventType, sessionformatv01.ForbidAssistant); err != nil {
				return err
			}
		}
		if mode == modeTarget && known {
			if err := assertV2Payload(event, viewOf(spec)); err != nil {
				return err
			}
		}
		if eventType == "session/end-seed" {
			data, err := decodeData(event)
			if err != nil {
				return err
			}
			if data["inherited"] == true {
				marker := int64(index)
				lastInheritedMarker = &marker
			}
		}
	}
	if seeded, _ := artifact.Header["isSeeded"].(bool); seeded &&
		(lastInheritedMarker == nil || *lastInheritedMarker != cut) {
		return sessionformat.FormatErrorf("format v2 seeded header disagrees with its last inherited end-seed marker")
	}
	if seeded, _ := artifact.Header["isSeeded"].(bool); !seeded && lastInheritedMarker != nil {
		return sessionformat.FormatErrorf("format v2 unseeded Session contains an inherited end-seed marker")
	}
	if mode == modeTarget {
		return sessionformatv01.AssertReleasedArtifactRelationships(artifact, RelationshipExtensions())
	}
	return nil
}

func decodeData(event sessionformat.Event) (map[string]any, error) {
	tree, err := sessionformat.DecodeValue(event.Data)
	if err != nil {
		return nil, sessionformat.FormatErrorf("%s %d data must be a JSON object", event.Type, event.Seq)
	}
	record, ok := tree.(map[string]any)
	if !ok {
		return nil, sessionformat.FormatErrorf("%s %d data must be a JSON object", event.Type, event.Seq)
	}
	return record, nil
}

// assertV2Payload validates one exact known v2 payload, re-assembling the
// embedded Assistant stream through the runtime llm faces.
func assertV2Payload(event sessionformat.Event, spec dispositionView) error {
	data, err := decodeData(event)
	if err != nil {
		return err
	}
	label := event.Type + " " + itoa(event.Seq)
	if err := sessionformatv01.AssertDataKeys(data, spec.Required, spec.Optional, label+" data"); err != nil {
		return err
	}
	for _, key := range spec.Opaque {
		if _, ok := data[key]; ok {
			if _, err := sessionformat.EncodeValue(data[key]); err != nil {
				return sessionformat.FormatErrorf("%s opaque %s is not lossless JSON", label, key)
			}
		}
	}
	if event.Type != "assistant/attempt" && event.Type != "assistant/message" {
		if event.Type == "session/end-seed" {
			if value, ok := data["inherited"]; ok && value != true {
				return sessionformat.FormatErrorf("session/end-seed %d inherited must be true when present", event.Seq)
			}
			return nil
		}
		return sessionformatv01.AssertReleasedPayloadSemanticsOnly(event, 2)
	}
	turn, err := sessionformat.CountField(data, "turn", label+" turn")
	if err != nil {
		return err
	}
	step, err := sessionformat.CountField(data, "step", label+" step")
	if err != nil {
		return err
	}
	streamValue, err := sessionformat.EncodeValue(data["stream"])
	if err != nil {
		return sessionformat.FormatErrorf("%s has an invalid embedded stream", label)
	}
	records, err := llm.ParseAssistantStream(streamValue)
	if err != nil {
		return sessionformat.FormatErrorf("%s has an invalid embedded stream: %s", label, err.Error())
	}
	timed, err := llm.ExpandAssistantStream(records)
	if err != nil {
		return sessionformat.FormatErrorf("%s has an invalid embedded stream: %s", label, err.Error())
	}
	assembler := llm.NewBlockAssembler()
	for _, member := range timed {
		chunkJSON, err := json.Marshal(member.Chunk)
		if err != nil {
			return sessionformat.FormatErrorf("%s has an invalid embedded stream", label)
		}
		chunkEvent := sessionformat.Event{
			Type: "assistant/chunk", Seq: event.Seq, Time: member.Time,
			Data: mustJSON(map[string]any{"turn": numberOf(turn), "step": numberOf(step), "chunk": mustDecode(chunkJSON)}),
		}
		if err := sessionformatv01.AssertReleasedPayloadSemanticsOnly(chunkEvent, 2); err != nil {
			return sessionformat.FormatErrorf("%s has an invalid embedded stream: %s", label, err.Error())
		}
		assembler.Push(member.Chunk)
	}
	if event.Type == "assistant/attempt" {
		return nil
	}
	if err := sessionformatv01.AssertReleasedPayloadSemanticsOnly(event, 2); err != nil {
		return err
	}
	if len(timed) > 0 {
		message, ok := data["message"].(map[string]any)
		if !ok {
			return sessionformat.FormatErrorf("assistant/message %d message must be a JSON object", event.Seq)
		}
		var content any
		if interrupted, _ := data["interrupted"].(bool); interrupted {
			content = blocksToJSON(assembler.InterruptedBlocks())
		} else {
			content = blocksToJSON(assembler.Blocks())
		}
		if !deepEqualJSON(message["content"], content) {
			return sessionformat.FormatErrorf("assistant/message %d message content disagrees with its embedded stream", event.Seq)
		}
		if !deepEqualJSON(data["usage"], usageToJSON(assembler.Usage())) {
			return sessionformat.FormatErrorf("assistant/message %d usage disagrees with its embedded stream", event.Seq)
		}
		source, ok := message["source"].(map[string]any)
		if !ok {
			return sessionformat.FormatErrorf("assistant/message %d source must be a JSON object", event.Seq)
		}
		if !deepEqualJSON(source["replayState"], replayToJSON(assembler.ReplayState())) {
			return sessionformat.FormatErrorf("assistant/message %d replay state disagrees with its embedded stream", event.Seq)
		}
	}
	return nil
}

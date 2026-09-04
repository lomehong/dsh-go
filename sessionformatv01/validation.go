package sessionformatv01

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"dshgo/sessionformat"
)

// jsonRawMessage aliases the raw JSON bytes for helper signatures.
type jsonRawMessage = json.RawMessage

func jsonUnmarshal(data []byte, target any) error { return json.Unmarshal(data, target) }

// Strict source and target validation for the released v0 and v1 logical
// generations. Port of packages/session/session-format-v0-to-v1/src/validation.ts.

var (
	headerRequired = []string{"version", "id", "createdAt", "isSeeded", "delegationDepth"}
	headerOptional = []string{"cwd", "parentSession", "origin", "agentPreset"}
	eventRequired  = []string{"type", "seq", "time", "data"}
	surfaceTypes   = map[string]bool{"user/message": true, "assistant/message": true, "tool/result": true}
	surfaceOptionl = []string{"ignorable", "sourceEventSeqs", "surfaceOp"}
	logOptional    = []string{"ignorable"}
	legacySources  = map[string]bool{"steering/message": true, "request/header-delta": true, "mode/set": true}
)

// coordinateMode selects how strict the event envelope check is.
type coordinateMode struct {
	// allowLegacySteering admits the legacy source types (source-v0 only).
	allowLegacySteering bool
	// frozenInventory pins the envelope to the released-v0 inventory shape
	// (official knownEventTypes === RELEASED_V0_EVENT_TYPE_SET).
	frozenInventory bool
	// vocabularyNeutral skips the known-type guard entirely (physical
	// layout validation).
	vocabularyNeutral bool
	// knownEventTypes is the installed current Session vocabulary.
	knownEventTypes func(string) bool
}

// AssertReleasedSessionFormatHeader validates the logical header shared by
// released v0 and v1 for one exact generation.
func AssertReleasedSessionFormatHeader(header sessionformat.Header, version int64) error {
	label := "format v" + itoa(version) + " header"
	if err := assertKeys(header, headerRequired, headerOptional, label); err != nil {
		return err
	}
	stored, err := sessionformat.HeaderVersion(header)
	if err != nil {
		return err
	}
	if stored != version {
		return sessionformat.FormatErrorf("expected format v%d header", version)
	}
	if _, ok := header["id"].(string); !ok {
		return sessionformat.FormatErrorf("%s id must be a string", label)
	}
	if _, err := sessionformat.CountField(header, "createdAt", label+" createdAt"); err != nil {
		return err
	}
	if _, ok := header["isSeeded"].(bool); !ok {
		return sessionformat.FormatErrorf("%s isSeeded must be a boolean", label)
	}
	if _, err := sessionformat.CountField(header, "delegationDepth", label+" delegationDepth"); err != nil {
		return err
	}
	for _, key := range []string{"cwd", "parentSession", "agentPreset"} {
		if value, ok := header[key]; ok && value != nil {
			if _, ok := value.(string); !ok {
				return sessionformat.FormatErrorf("%s %s must be a string", label, key)
			}
		}
	}
	if cwd, ok := header["cwd"].(string); ok {
		if !filepath.IsAbs(cwd) && !isWindowsAbsolute(cwd) {
			return sessionformat.FormatErrorf("%s cwd must be absolute", label)
		}
	}
	if origin, ok := header["origin"]; ok && origin != nil {
		if origin != "subagent" {
			return sessionformat.FormatErrorf("%s origin must be \"subagent\"", label)
		}
	}
	return nil
}

// isWindowsAbsolute admits drive-letter and UNC absolute paths (Go's
// filepath.IsAbs is platform-relative; the official check is node's
// win32-aware isAbsolute at validation time on any host).
func isWindowsAbsolute(path string) bool {
	if len(path) >= 3 && path[1] == ':' && (path[2] == '\\' || path[2] == '/') {
		return true
	}
	return strings.HasPrefix(path, `\\`) || strings.HasPrefix(path, `//`)
}

// AssertReleasedV1Header validates one released-v1 logical header.
func AssertReleasedV1Header(header sessionformat.Header) error {
	return AssertReleasedSessionFormatHeader(header, 1)
}

// AssertReleasedV0SourceArtifact validates v0 before historical normalizers
// run.
func AssertReleasedV0SourceArtifact(artifact sessionformat.Artifact) error {
	if err := AssertReleasedSessionFormatHeader(artifact.Header, 0); err != nil {
		return err
	}
	return assertArtifactCoordinates(artifact, coordinateMode{allowLegacySteering: true, frozenInventory: true})
}

// AssertNormalizedReleasedV0Artifact validates normalized v0 events before
// the identity header version changes.
func AssertNormalizedReleasedV0Artifact(artifact sessionformat.Artifact) error {
	if err := AssertReleasedSessionFormatHeader(artifact.Header, 0); err != nil {
		return err
	}
	if err := assertArtifactCoordinates(artifact, coordinateMode{frozenInventory: true}); err != nil {
		return err
	}
	for _, event := range artifact.Events {
		if err := AssertReleasedEventPayload(event, 0); err != nil {
			return err
		}
	}
	return AssertReleasedArtifactRelationships(artifact, RelationshipExtensions{})
}

// AssertReleasedV1Artifact validates the exact logical image emitted by the
// released v1 writer.
func AssertReleasedV1Artifact(artifact sessionformat.Artifact) error {
	if err := AssertReleasedV1Header(artifact.Header); err != nil {
		return err
	}
	if err := assertArtifactCoordinates(artifact, coordinateMode{frozenInventory: true}); err != nil {
		return err
	}
	for _, event := range artifact.Events {
		if _, known := dispositionFor(event.Type); known {
			if err := AssertReleasedEventPayload(event, 1); err != nil {
				return err
			}
		}
	}
	return AssertReleasedArtifactRelationships(artifact, RelationshipExtensions{})
}

// RestoreReleasedV1Artifact restores v1 against the installed build's
// ordinary event vocabulary without freezing payload additions.
func RestoreReleasedV1Artifact(artifact sessionformat.Artifact, knownEventTypes func(string) bool) error {
	if err := AssertReleasedV1Header(artifact.Header); err != nil {
		return err
	}
	return assertArtifactCoordinates(artifact, coordinateMode{knownEventTypes: knownEventTypes})
}

// AssertReleasedV1PhysicalArtifact validates released-v1 physical layout
// without interpreting event vocabulary.
func AssertReleasedV1PhysicalArtifact(artifact sessionformat.Artifact) error {
	if err := AssertReleasedV1Header(artifact.Header); err != nil {
		return err
	}
	return assertArtifactCoordinates(artifact, coordinateMode{vocabularyNeutral: true})
}

func assertArtifactCoordinates(artifact sessionformat.Artifact, mode coordinateMode) error {
	inherited := artifact.InheritedEventCount
	if inherited > int64(len(artifact.Events)) {
		return sessionformat.FormatErrorf("Session inheritedEventCount exceeds its event count")
	}
	if seeded, _ := artifact.Header["isSeeded"].(bool); !seeded && inherited != 0 {
		return sessionformat.FormatErrorf("unseeded Session inheritedEventCount must be 0")
	}
	for index, event := range artifact.Events {
		eventType := event.Type
		_, known := dispositionFor(eventType)
		legacy := mode.allowLegacySteering && legacySources[eventType]
		currentKnown := known || (mode.knownEventTypes != nil && mode.knownEventTypes(eventType))
		ignorableValue, hasIgnorable := event.ExtraField("ignorable")
		ignorable := false
		if hasIgnorable {
			var flag bool
			if err := unmarshalStrict(ignorableValue, &flag); err != nil || !flag {
				return sessionformat.FormatErrorf("Session event %d ignorable must be true when present", index)
			}
			ignorable = true
		}
		ignorableCurrent := !mode.allowLegacySteering && !currentKnown && ignorable
		if !currentKnown && !legacy && !ignorableCurrent && !mode.vocabularyNeutral {
			if mode.allowLegacySteering {
				return sessionformat.UnsupportedErrorf(
					"format v0 contains unknown historical event type %q at seq %d; migration refuses unknown historical events even when ignorable",
					eventType, index)
			}
			return sessionformat.UnsupportedErrorf(
				"format v1 contains unknown required event type %q at seq %d", eventType, index)
		}
		surface := false
		if _, known := dispositionFor(eventType); known {
			surface = surfaceTypes[eventType]
		} else {
			surface = eventType == "steering/message"
		}
		optional := surfaceOptionl
		if mode.frozenInventory && !surface {
			optional = logOptional
		}
		if err := assertEnvelopeKeys(event, optional, index); err != nil {
			return err
		}
		if event.Seq != int64(index) {
			return sessionformat.FormatErrorf("Session event %d has non-dense seq %d", index, event.Seq)
		}
		if mode.frozenInventory && surface {
			if err := AssertReleasedSurfaceMetadata(event, int64(index), eventType, allowEmptyAssistant); err != nil {
				return err
			}
		}
	}
	return nil
}

// assertEnvelopeKeys requires exactly type/seq/time/data plus the optional
// envelope members.
func assertEnvelopeKeys(event sessionformat.Event, optional []string, index int) error {
	allowed := map[string]bool{"type": true, "seq": true, "time": true, "data": true}
	for _, key := range optional {
		allowed[key] = true
	}
	for key := range event.Extra {
		if !allowed[key] {
			return sessionformat.FormatErrorf("Session event %d has unexpected member %q", index, key)
		}
	}
	return nil
}

// AssistantSources selects whether a generation admits empty Assistant
// chunk provenance.
type AssistantSources string

// Assistant provenance policies.
const (
	// AllowEmptyAssistant admits a present empty sourceEventSeqs on
	// assistant/message (a known empty provider stream).
	AllowEmptyAssistant AssistantSources = "allow-empty-assistant"
	// ForbidAssistant refuses assistant/message chunk provenance (obsolete
	// under format v2).
	ForbidAssistant AssistantSources = "forbid-assistant"
)

const allowEmptyAssistant = AllowEmptyAssistant

// AssertReleasedSurfaceMetadata validates shared-layout surface references
// for one released generation.
func AssertReleasedSurfaceMetadata(event sessionformat.Event, seq int64, eventType string, assistantSources AssistantSources) error {
	sourcesRaw, hasSources := event.ExtraField("sourceEventSeqs")
	if eventType == "assistant/message" && hasSources && assistantSources == ForbidAssistant {
		return sessionformat.FormatErrorf("assistant/message %d retains obsolete chunk provenance", seq)
	}
	if hasSources {
		tree, err := sessionformat.DecodeValue(sourcesRaw)
		if err != nil {
			return sessionformat.FormatErrorf("%s %d sourceEventSeqs must be an array", eventType, seq)
		}
		members, ok := tree.([]any)
		if !ok {
			return sessionformat.FormatErrorf("%s %d sourceEventSeqs must be an array", eventType, seq)
		}
		seen := map[int64]bool{}
		for _, member := range members {
			current, err := sessionformat.Count(member, eventType+" "+itoa(seq)+" sourceEventSeqs member")
			if err != nil {
				return err
			}
			if current >= seq || seen[current] {
				return sessionformat.FormatErrorf("%s %d sourceEventSeqs must be unique earlier seqs", eventType, seq)
			}
			seen[current] = true
		}
		if len(members) == 0 && (eventType != "assistant/message" || assistantSources == ForbidAssistant) {
			return sessionformat.FormatErrorf("%s %d sourceEventSeqs must be non-empty", eventType, seq)
		}
	}
	operationRaw, hasOperation := event.ExtraField("surfaceOp")
	if !hasOperation {
		return nil
	}
	if string(operationRaw) == `"append"` {
		return nil
	}
	tree, err := sessionformat.DecodeValue(operationRaw)
	if err != nil {
		return sessionformat.FormatErrorf("%s %d surfaceOp must be a JSON object", eventType, seq)
	}
	replacement, err := recordOf(tree, eventType+" "+itoa(seq)+" surfaceOp")
	if err != nil {
		return err
	}
	if err := assertKeys(replacement, []string{"op", "start", "end"}, nil, eventType+" "+itoa(seq)+" surfaceOp"); err != nil {
		return err
	}
	if replacement["op"] != "replace" {
		return sessionformat.FormatErrorf("%s %d surfaceOp must replace", eventType, seq)
	}
	start, err := sessionformat.Count(replacement["start"], eventType+" "+itoa(seq)+" surface start")
	if err != nil {
		return err
	}
	end, err := sessionformat.Count(replacement["end"], eventType+" "+itoa(seq)+" surface end")
	if err != nil {
		return err
	}
	if start >= seq || end >= seq {
		return sessionformat.FormatErrorf("%s %d has an invalid surface replacement", eventType, seq)
	}
	return nil
}

// AssertReleasedEventPayload validates one exact known payload after legacy
// normalization.
func AssertReleasedEventPayload(event sessionformat.Event, version int64) error {
	spec, known := dispositionFor(event.Type)
	if !known {
		return sessionformat.UnsupportedErrorf(
			"format v0 contains unknown event type %q at seq %d", event.Type, event.Seq)
	}
	label := event.Type + " " + itoa(event.Seq)
	data, err := decodeData(event, label)
	if err != nil {
		return err
	}
	if event.Type == "subagent/descriptor" {
		descriptorVersion, versionErr := sessionformat.CountField(data, "version", label+" version")
		if versionErr != nil {
			return versionErr
		}
		if descriptorVersion != 3 {
			if version == 0 {
				return sessionformat.UnsupportedErrorf(
					"%s uses unsupported descriptor version %d", event.Type, descriptorVersion)
			}
			return nil
		}
	}
	versionOptional := spec.optional
	if version == 1 && event.Type == "session-log-deepseek/delivery-accepted" {
		versionOptional = append(append([]string{}, spec.optional...), "sessionFormatVersion")
	}
	if err := assertKeys(data, spec.required, versionOptional, label+" data"); err != nil {
		return err
	}
	for _, key := range spec.opaque {
		if value, ok := data[key]; ok {
			if _, err := sessionformat.EncodeValue(value); err != nil {
				return sessionformat.FormatErrorf("%s opaque %s is not lossless JSON", label, key)
			}
		}
	}
	return assertReleasedPayloadSemantics(event, version, data, label)
}

func itoa(value int64) string {
	digits := ""
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	for value > 0 {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
	}
	if negative {
		return "-" + digits
	}
	return digits
}

// unmarshalStrict decodes one raw member into a typed value, rejecting
// trailing garbage.
func unmarshalStrict(raw jsonRawMessage, target any) error {
	return jsonUnmarshal(raw, target)
}

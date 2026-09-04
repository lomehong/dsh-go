package sessionformatv12

import (
	"encoding/json"

	"dshgo/sessionformat"
	"dshgo/sessionformatv01"
)

// Frozen physical JSON codec for released v2. Port of
// packages/session/session-format-v1-to-v2/src/codec.ts: one durable event
// per physical row, range-encoded sourceEventSeqs, and the seed cut derived
// from inherited end-seed markers (the header stores isSeeded but no
// numeric cut).

// ReleasedV2Codec is the frozen physical codec for released v2.
func ReleasedV2Codec() *ReleasedCodec { return &ReleasedCodec{} }

// ReleasedCodec implements sessionformat.Codec for format v2.
type ReleasedCodec struct{}

// Version implements sessionformat.Codec.
func (*ReleasedCodec) Version() int64 { return 2 }

// DecodeHeader implements sessionformat.Codec.
func (*ReleasedCodec) DecodeHeader(headerValue json.RawMessage) (sessionformat.Header, error) {
	return decodePhysicalHeader(headerValue)
}

// DecodeArtifact implements sessionformat.Codec.
func (c *ReleasedCodec) DecodeArtifact(headerValue json.RawMessage, rows []json.RawMessage) (sessionformat.Artifact, error) {
	return c.decodeArtifact(headerValue, rows, false)
}

// DecodeRecoverableArtifact implements sessionformat.Codec.
func (c *ReleasedCodec) DecodeRecoverableArtifact(headerValue json.RawMessage, rows []json.RawMessage) (sessionformat.Artifact, error) {
	return c.decodeArtifact(headerValue, rows, true)
}

// EncodeArtifact physically encodes one validated current artifact.
func (*ReleasedCodec) EncodeArtifact(artifact sessionformat.Artifact) (sessionformat.EncodedArtifact, error) {
	if err := AssertReleasedV2PhysicalArtifact(artifact); err != nil {
		return sessionformat.EncodedArtifact{}, err
	}
	return encodeArtifact(artifact)
}

func decodePhysicalHeader(value json.RawMessage) (sessionformat.Header, error) {
	header, err := sessionformat.DecodeHeaderValue(value)
	if err != nil {
		return nil, sessionformat.FormatErrorf("released v2 physical header must be a JSON object")
	}
	if err := sessionformatv01AssertKeys(header); err != nil {
		return nil, err
	}
	if header["type"] != "session" {
		return nil, sessionformat.FormatErrorf("expected released v2 physical Session header")
	}
	storedVersion, err := sessionformat.HeaderVersion(header)
	if err != nil {
		return nil, err
	}
	if storedVersion != 2 {
		return nil, sessionformat.FormatErrorf("expected released v2 physical Session header")
	}
	if _, ok := header["id"].(string); !ok {
		return nil, sessionformat.FormatErrorf("released v2 header id must be a string")
	}
	createdAt, err := sessionformat.CountField(header, "createdAt", "released v2 header createdAt")
	if err != nil {
		return nil, err
	}
	delegationDepth, err := sessionformat.CountField(header, "delegationDepth", "released v2 header delegationDepth")
	if err != nil {
		return nil, err
	}
	if _, ok := header["isSeeded"].(bool); !ok {
		return nil, sessionformat.FormatErrorf("released v2 header isSeeded must be boolean")
	}
	for _, key := range []string{"cwd", "parentSession", "agentPreset"} {
		if member, ok := header[key]; ok && member != nil {
			if _, isString := member.(string); !isString {
				return nil, sessionformat.FormatErrorf("released v2 header %s must be a string", key)
			}
		}
	}
	if origin, ok := header["origin"]; ok && origin != nil && origin != "subagent" {
		return nil, sessionformat.FormatErrorf("released v2 header origin must be \"subagent\"")
	}
	logical := sessionformat.Header{
		"version":         numberOf(2),
		"id":              header["id"],
		"createdAt":       numberOf(createdAt),
		"isSeeded":        header["isSeeded"],
		"delegationDepth": numberOf(delegationDepth),
	}
	for _, key := range []string{"cwd", "parentSession", "origin", "agentPreset"} {
		if member, ok := header[key]; ok {
			logical[key] = member
		}
	}
	if err := AssertReleasedV2Header(logical); err != nil {
		return nil, err
	}
	return logical, nil
}

func (c *ReleasedCodec) decodeArtifact(headerValue json.RawMessage, rows []json.RawMessage, recoverable bool) (sessionformat.Artifact, error) {
	header, err := decodePhysicalHeader(headerValue)
	if err != nil {
		return sessionformat.Artifact{}, err
	}
	events := []sessionformat.Event{}
	var issue error
	for rowIndex, value := range rows {
		event, eventErr := c.decodeEvent(value, rowIndex)
		if eventErr != nil {
			if !recoverable {
				return sessionformat.Artifact{}, eventErr
			}
			if issue == nil {
				issue = eventErr
			}
			continue
		}
		if issue != nil {
			if event.Type == "turn/end" {
				return sessionformat.Artifact{}, issue
			}
			continue
		}
		if event.Seq != int64(len(events)) {
			gap := sessionformat.FormatErrorf(
				"released v2 row %d has seq gap (expected %d, got %d)", rowIndex, len(events), event.Seq)
			if !recoverable {
				return sessionformat.Artifact{}, gap
			}
			issue = gap
			if event.Type == "turn/end" {
				return sessionformat.Artifact{}, issue
			}
			continue
		}
		events = append(events, event)
	}
	cut, err := deriveInheritedEventCount(header, events)
	if err != nil {
		return sessionformat.Artifact{}, err
	}
	artifact, err := sessionformat.SnapshotArtifact(
		sessionformat.Artifact{Header: header, InheritedEventCount: cut, Events: events},
		"released v2 artifact")
	if err != nil {
		return sessionformat.Artifact{}, err
	}
	if err := AssertReleasedV2PhysicalArtifact(artifact); err != nil {
		return sessionformat.Artifact{}, err
	}
	return artifact, nil
}

func (*ReleasedCodec) decodeEvent(value json.RawMessage, rowIndex int) (sessionformat.Event, error) {
	var event sessionformat.Event
	if err := json.Unmarshal(value, &event); err != nil {
		return event, sessionformat.FormatErrorf("released v2 row %d is malformed", rowIndex)
	}
	if raw, ok := event.ExtraField("sourceEventSeqs"); ok {
		tree, err := sessionformat.DecodeValue(raw)
		if err != nil {
			return event, sessionformat.FormatErrorf("sourceEventSeqs must be an array")
		}
		decoded, err := decodeSeqRanges(tree, event.Seq)
		if err != nil {
			return event, err
		}
		data, err := sessionformat.EncodeValue(decoded)
		if err != nil {
			return event, err
		}
		event = setEnvelopeExtra(event, "sourceEventSeqs", data)
	}
	return event, nil
}

func deriveInheritedEventCount(header sessionformat.Header, events []sessionformat.Event) (int64, error) {
	var cut *int64
	for _, event := range events {
		if event.Type != "session/end-seed" {
			continue
		}
		data, err := decodeData(event)
		if err != nil {
			return 0, err
		}
		if data["inherited"] == true {
			seq := event.Seq
			cut = &seq
		}
	}
	seeded, _ := header["isSeeded"].(bool)
	if seeded && cut == nil {
		return 0, sessionformat.FormatErrorf("released v2 seeded Session lacks an inherited end-seed marker")
	}
	if !seeded && cut != nil {
		return 0, sessionformat.FormatErrorf("released v2 unseeded Session contains an inherited end-seed marker")
	}
	if cut == nil {
		return 0, nil
	}
	return *cut, nil
}

func encodeArtifact(artifact sessionformat.Artifact) (sessionformat.EncodedArtifact, error) {
	header := artifact.Header
	physicalHeader := map[string]any{
		"type":            "session",
		"version":         numberOf(2),
		"id":              header["id"],
		"createdAt":       header["createdAt"],
		"isSeeded":        header["isSeeded"],
		"delegationDepth": header["delegationDepth"],
	}
	for _, key := range []string{"cwd", "parentSession", "origin", "agentPreset"} {
		if value, ok := header[key]; ok {
			physicalHeader[key] = value
		}
	}
	headerJSON, err := sessionformat.EncodeValue(physicalHeader)
	if err != nil {
		return sessionformat.EncodedArtifact{}, err
	}
	rows := make([]json.RawMessage, 0, len(artifact.Events))
	for _, event := range artifact.Events {
		raw, err := json.Marshal(event)
		if err != nil {
			return sessionformat.EncodedArtifact{}, err
		}
		encoded, err := encodeProvenance(raw)
		if err != nil {
			return sessionformat.EncodedArtifact{}, err
		}
		rows = append(rows, encoded)
	}
	return sessionformat.EncodedArtifact{Header: headerJSON, Rows: rows}, nil
}

func encodeProvenance(eventRaw json.RawMessage) (json.RawMessage, error) {
	tree, err := sessionformat.DecodeValue(eventRaw)
	if err != nil {
		return nil, err
	}
	record, ok := tree.(map[string]any)
	if !ok {
		return nil, sessionformat.FormatErrorf("released v2 event must be an object")
	}
	sources, hasSources := record["sourceEventSeqs"]
	if !hasSources {
		return encodeTree(record)
	}
	values, err := flatSeqList(sources)
	if err != nil {
		return nil, err
	}
	record["sourceEventSeqs"] = encodeSeqRanges(values)
	return encodeTree(record)
}

func flatSeqList(value any) ([]int64, error) {
	members, ok := value.([]any)
	if !ok {
		return nil, sessionformat.FormatErrorf("sourceEventSeqs must be an array")
	}
	output := make([]int64, 0, len(members))
	for _, member := range members {
		seq, err := sessionformat.Count(member, "sourceEventSeqs member")
		if err != nil {
			return nil, err
		}
		output = append(output, seq)
	}
	return output, nil
}

func decodeSeqRanges(value any, maxEntries int64) ([]int64, error) {
	members, ok := value.([]any)
	if !ok {
		return nil, sessionformat.FormatErrorf("sourceEventSeqs must be an array")
	}
	var output []int64
	hasRange := false
	for _, entry := range members {
		switch typed := entry.(type) {
		case []any:
			if len(typed) != 2 {
				return nil, sessionformat.FormatErrorf("sourceEventSeqs range must be a [start, end] pair")
			}
			start, err := sessionformat.Count(typed[0], "sourceEventSeqs range start")
			if err != nil {
				return nil, err
			}
			end, err := sessionformat.Count(typed[1], "sourceEventSeqs range end")
			if err != nil {
				return nil, err
			}
			if start > end || end >= maxEntries || end-start+1 > maxEntries-int64(len(output)) {
				return nil, sessionformat.FormatErrorf("sourceEventSeqs range exceeds its event seq")
			}
			for seq := start; seq <= end; seq++ {
				output = append(output, seq)
			}
			hasRange = true
		default:
			seq, err := sessionformat.Count(typed, "sourceEventSeqs member")
			if err != nil {
				return nil, err
			}
			output = append(output, seq)
		}
	}
	seen := map[int64]bool{}
	for _, source := range output {
		if source >= maxEntries || seen[source] {
			return nil, sessionformat.FormatErrorf("sourceEventSeqs ranges must contain unique earlier seqs")
		}
		seen[source] = true
	}
	if hasRange {
		for index := 1; index < len(output); index++ {
			if output[index] <= output[index-1] {
				return nil, sessionformat.FormatErrorf("sourceEventSeqs ranges must be strictly increasing")
			}
		}
	}
	return output, nil
}

func encodeSeqRanges(values []int64) []any {
	for index := 1; index < len(values); index++ {
		if values[index] <= values[index-1] {
			output := make([]any, 0, len(values))
			for _, value := range values {
				output = append(output, numberOf(value))
			}
			return output
		}
	}
	output := []any{}
	for index := 0; index < len(values); {
		start := values[index]
		end := start
		for index+1 < len(values) && values[index+1] == end+1 {
			index++
			end++
		}
		if end-start >= 2 {
			output = append(output, []any{numberOf(start), numberOf(end)})
		} else {
			output = append(output, numberOf(start))
			if end-start == 1 {
				output = append(output, numberOf(end))
			}
		}
		index++
	}
	return output
}

func encodeTree(value any) (json.RawMessage, error) {
	return sessionformat.EncodeValue(value)
}

func setEnvelopeExtra(event sessionformat.Event, key string, rawValue json.RawMessage) sessionformat.Event {
	if event.Extra == nil {
		event.Extra = map[string]json.RawMessage{}
	}
	event.Extra[key] = rawValue
	return event
}

func sessionformatv01AssertKeys(header sessionformat.Header) error {
	return sessionformatv01.AssertKeys(header, physicalHeaderRequired, physicalHeaderOptional, "released v2 physical header")
}

var (
	physicalHeaderRequired = []string{"type", "version", "id", "createdAt", "isSeeded", "delegationDepth"}
	physicalHeaderOptional = []string{"cwd", "parentSession", "origin", "agentPreset"}
)

package sessionformatv01

import (
	"encoding/json"

	"dshgo/sessionformat"
)

// Physical JSON codecs for the released v0 and v1 layouts. Port of
// packages/session/session-format-v0-to-v1/src/codec.ts.

var (
	physicalHeaderRequired = []string{"type", "version", "id", "createdAt", "delegationDepth"}
	physicalHeaderOptional = []string{"cwd", "parentSession", "seedLength", "origin", "agentPreset"}
	packedTags             = map[string]bool{"text-chunks": true, "reasoning-chunks": true, "tool-call-chunks": true}
)

// EncodeOptions tune physical encoding (official SessionFormatEncodeOptions).
type EncodeOptions struct {
	// PackChunks packs eligible assistant/chunk runs into packed rows.
	PackChunks bool
}

// ReleasedV0Codec is the frozen physical JSON codec for the released v0
// layout.
func ReleasedV0Codec() sessionformat.Codec { return &releasedCodec{version: 0} }

// ReleasedV1Codec is the frozen physical JSON codec for the shared-layout
// released v1 format.
func ReleasedV1Codec() sessionformat.Codec { return &releasedCodec{version: 1} }

type releasedCodec struct{ version int64 }

func (c *releasedCodec) Version() int64 { return c.version }

func (c *releasedCodec) DecodeHeader(headerValue json.RawMessage) (sessionformat.Header, error) {
	physical, err := decodePhysicalHeader(headerValue, c.version)
	return physical.header, err
}

func (c *releasedCodec) DecodeArtifact(headerValue json.RawMessage, rows []json.RawMessage) (sessionformat.Artifact, error) {
	physical, err := decodePhysicalHeader(headerValue, c.version)
	if err != nil {
		return sessionformat.Artifact{}, err
	}
	events, err := scanRows(rows, false)
	if err != nil {
		return sessionformat.Artifact{}, err
	}
	artifact := sessionformat.Artifact{Header: physical.header, InheritedEventCount: physical.inheritedEventCount, Events: events}
	artifact, err = sessionformat.SnapshotArtifact(artifact, "released v"+itoa(c.version)+" artifact")
	if err != nil {
		return sessionformat.Artifact{}, err
	}
	if c.version == 0 {
		if err := AssertReleasedV0SourceArtifact(artifact); err != nil {
			return sessionformat.Artifact{}, err
		}
	} else if err := AssertReleasedV1PhysicalArtifact(artifact); err != nil {
		return sessionformat.Artifact{}, err
	}
	return artifact, nil
}

func (c *releasedCodec) DecodeRecoverableArtifact(headerValue json.RawMessage, rows []json.RawMessage) (sessionformat.Artifact, error) {
	physical, err := decodePhysicalHeader(headerValue, c.version)
	if err != nil {
		return sessionformat.Artifact{}, err
	}
	events, err := scanRows(rows, true)
	if err != nil {
		return sessionformat.Artifact{}, err
	}
	artifact := sessionformat.Artifact{Header: physical.header, InheritedEventCount: physical.inheritedEventCount, Events: events}
	artifact, err = sessionformat.SnapshotArtifact(artifact, "released v"+itoa(c.version)+" recoverable artifact")
	if err != nil {
		return sessionformat.Artifact{}, err
	}
	if c.version == 0 {
		if err := AssertReleasedV0SourceArtifact(artifact); err != nil {
			return sessionformat.Artifact{}, err
		}
	} else if err := AssertReleasedV1PhysicalArtifact(artifact); err != nil {
		return sessionformat.Artifact{}, err
	}
	return artifact, nil
}

type physicalHeader struct {
	header              sessionformat.Header
	inheritedEventCount int64
}

func decodePhysicalHeader(value json.RawMessage, version int64) (physicalHeader, error) {
	label := "released v" + itoa(version) + " physical header"
	header, err := sessionformat.DecodeHeaderValue(value)
	if err != nil {
		return physicalHeader{}, sessionformat.FormatErrorf("%s must be a JSON object", label)
	}
	if err := assertKeys(header, physicalHeaderRequired, physicalHeaderOptional, label); err != nil {
		return physicalHeader{}, err
	}
	if header["type"] != "session" {
		return physicalHeader{}, sessionformat.FormatErrorf("expected released v%d physical Session header", version)
	}
	storedVersion, err := sessionformat.HeaderVersion(header)
	if err != nil {
		return physicalHeader{}, err
	}
	if storedVersion != version {
		return physicalHeader{}, sessionformat.FormatErrorf("expected released v%d physical Session header", version)
	}
	if _, ok := header["id"].(string); !ok {
		return physicalHeader{}, sessionformat.FormatErrorf("released v%d header id must be a string", version)
	}
	createdAt, err := sessionformat.CountField(header, "createdAt", label+" createdAt")
	if err != nil {
		return physicalHeader{}, err
	}
	delegationDepth, err := sessionformat.CountField(header, "delegationDepth", label+" delegationDepth")
	if err != nil {
		return physicalHeader{}, err
	}
	var seedLength int64
	hasSeed := false
	if value, ok := header["seedLength"]; ok {
		seedLength, err = sessionformat.Count(value, label+" seedLength")
		if err != nil {
			return physicalHeader{}, err
		}
		hasSeed = true
	}
	for _, key := range []string{"cwd", "parentSession", "agentPreset"} {
		if member, ok := header[key]; ok && member != nil {
			if _, isString := member.(string); !isString {
				return physicalHeader{}, sessionformat.FormatErrorf("released v%d header %s must be a string", version, key)
			}
		}
	}
	if origin, ok := header["origin"]; ok && origin != nil && origin != "subagent" {
		return physicalHeader{}, sessionformat.FormatErrorf("released v%d header origin must be \"subagent\"", version)
	}
	logical := sessionformat.Header{
		"version":         mustDecodeNumber(itoa(version)),
		"id":              header["id"],
		"createdAt":       mustDecodeNumber(itoa(createdAt)),
		"isSeeded":        hasSeed,
		"delegationDepth": mustDecodeNumber(itoa(delegationDepth)),
	}
	if cwd, ok := header["cwd"]; ok {
		logical["cwd"] = cwd
	}
	if parent, ok := header["parentSession"]; ok {
		logical["parentSession"] = parent
	}
	if origin, ok := header["origin"]; ok {
		logical["origin"] = origin
	}
	if preset, ok := header["agentPreset"]; ok {
		logical["agentPreset"] = preset
	}
	if _, err := sessionformat.SnapshotHeader(logical, "released v"+itoa(version)+" logical header"); err != nil {
		return physicalHeader{}, err
	}
	if err := AssertReleasedSessionFormatHeader(logical, version); err != nil {
		return physicalHeader{}, err
	}
	return physicalHeader{header: logical, inheritedEventCount: seedLength}, nil
}

// scanRows decodes physical rows into logical events, optionally recovering
// the longest valid prefix of a torn artifact.
func scanRows(rowValues []json.RawMessage, recoverable bool) ([]sessionformat.Event, error) {
	events := []sessionformat.Event{}
	var issue error
	for rowIndex, value := range rowValues {
		decoded, err := decodeRow(value, rowIndex)
		if err != nil {
			if !recoverable {
				return nil, err
			}
			if issue == nil {
				issue = err
			}
			continue
		}
		if issue != nil {
			if decodedContainsTurnEnd(decoded) {
				return nil, issue
			}
			continue
		}
		rowStart := len(events)
		abort := false
		for _, event := range decoded {
			if event.Seq != int64(len(events)) {
				gap := sessionformat.FormatErrorf(
					"released Session row %d has seq gap (expected %d, got %d)", rowIndex, len(events), event.Seq)
				events = events[:rowStart]
				if !recoverable {
					return nil, gap
				}
				issue = gap
				abort = true
				break
			}
			events = append(events, event)
		}
		if abort && issue != nil && decodedContainsTurnEnd(decoded) {
			return nil, issue
		}
		if issue != nil && !abort {
			if decodedContainsTurnEnd(decoded) {
				return nil, issue
			}
		}
	}
	return events, nil
}

func decodedContainsTurnEnd(events []sessionformat.Event) bool {
	for _, event := range events {
		if event.Type == "turn/end" {
			return true
		}
	}
	return false
}

func decodeRow(value json.RawMessage, rowIndex int) ([]sessionformat.Event, error) {
	tree, err := sessionformat.DecodeValue(value)
	if err != nil {
		return nil, sessionformat.FormatErrorf("released Session row %d is malformed", rowIndex)
	}
	record, err := recordOf(tree, "released Session row "+itoa(int64(rowIndex)))
	if err != nil {
		return nil, err
	}
	rowType, _ := record["type"].(string)
	if packedTags[rowType] {
		return expandPackedRow(record, rowType, rowIndex)
	}
	if sourcesValue, ok := record["sourceEventSeqs"]; ok {
		seq, err := countValue(record["seq"], "released Session row "+itoa(int64(rowIndex))+" seq")
		if err != nil {
			return nil, err
		}
		decoded, err := decodeSeqRanges(sourcesValue, seq)
		if err != nil {
			return nil, err
		}
		record["sourceEventSeqs"] = decoded
		event, err := recordToEvent(record)
		if err != nil {
			return nil, err
		}
		return []sessionformat.Event{event}, nil
	}
	event, err := recordToEvent(record)
	if err != nil {
		return nil, err
	}
	return []sessionformat.Event{event}, nil
}

// recordToEvent converts one decoded row record into the generic Event
// envelope, splitting interpreted from extra members.
func recordToEvent(record map[string]any) (sessionformat.Event, error) {
	var event sessionformat.Event
	raw, err := encodeTree(record)
	if err != nil {
		return event, err
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		return event, sessionformat.FormatErrorf("released Session row: %s", err.Error())
	}
	return event, nil
}

func expandPackedRow(row map[string]any, rowType string, rowIndex int) ([]sessionformat.Event, error) {
	label := "released " + rowType + " row " + itoa(int64(rowIndex))
	if err := assertKeys(row, []string{"type", "seq0", "time0", "data"}, nil, label); err != nil {
		return nil, err
	}
	seq0, err := countValue(row["seq0"], label+" seq0")
	if err != nil {
		return nil, err
	}
	time0, err := sessionformat.SafeInteger(row["time0"], label+" time0")
	if err != nil {
		return nil, err
	}
	data, err := recordOf(row["data"], label+" data")
	if err != nil {
		return nil, err
	}
	isTool := rowType == "tool-call-chunks"
	if isTool {
		if err := assertKeys(data, []string{"turn", "step", "index", "id", "dt", "args"}, []string{"name"}, label+" data"); err != nil {
			return nil, err
		}
	} else if err := assertKeys(data, []string{"turn", "step", "index", "dt", "texts"}, nil, label+" data"); err != nil {
		return nil, err
	}
	payloadKey := "texts"
	if isTool {
		payloadKey = "args"
	}
	payload, ok := data[payloadKey].([]any)
	if !ok || len(payload) == 0 {
		return nil, sessionformat.FormatErrorf("%s payload must be a non-empty string array", label)
	}
	for _, member := range payload {
		if _, ok := member.(string); !ok {
			return nil, sessionformat.FormatErrorf("%s payload must be a non-empty string array", label)
		}
	}
	gaps, ok := data["dt"].([]any)
	if !ok || len(gaps) != len(payload)-1 {
		return nil, sessionformat.FormatErrorf("%s dt length must match its payload", label)
	}
	gapValues := make([]int64, 0, len(gaps))
	for _, gap := range gaps {
		parsed, err := sessionformat.SafeInteger(gap, label+" dt member")
		if err != nil {
			return nil, err
		}
		gapValues = append(gapValues, parsed)
	}
	if _, err := countValue(data["turn"], label+" turn"); err != nil {
		return nil, sessionformat.FormatErrorf("%s turn, step, and index must be numbers", label)
	}
	if _, err := countValue(data["step"], label+" step"); err != nil {
		return nil, sessionformat.FormatErrorf("%s turn, step, and index must be numbers", label)
	}
	if _, err := countValue(data["index"], label+" index"); err != nil {
		return nil, sessionformat.FormatErrorf("%s turn, step, and index must be numbers", label)
	}
	if isTool {
		if _, ok := data["id"].(string); !ok || len(data["id"].(string)) == 0 {
			return nil, sessionformat.FormatErrorf("%s id and optional name must be strings", label)
		}
		if name, ok := data["name"]; ok {
			if _, isString := name.(string); !isString {
				return nil, sessionformat.FormatErrorf("%s id and optional name must be strings", label)
			}
		}
	}
	output := make([]sessionformat.Event, 0, len(payload))
	time := time0
	for index, member := range payload {
		if index > 0 {
			time += gapValues[index-1]
		}
		text, _ := member.(string)
		chunkType := rowType
		var chunk map[string]any
		switch chunkType {
		case "text-chunks":
			chunk = map[string]any{"type": "text-delta", "index": data["index"], "text": text}
		case "reasoning-chunks":
			chunk = map[string]any{"type": "reasoning-delta", "index": data["index"], "text": text}
		default:
			chunk = map[string]any{"type": "tool-call-delta", "index": data["index"], "id": data["id"], "argumentsDelta": text}
			if name, ok := data["name"]; ok {
				chunk["name"] = name
			}
		}
		record := map[string]any{
			"type": "assistant/chunk",
			"seq":  mustDecodeNumber(itoa(seq0 + int64(index))),
			"time": mustDecodeNumber(itoa(time)),
			"data": map[string]any{"turn": data["turn"], "step": data["step"], "chunk": chunk},
		}
		event, err := recordToEvent(record)
		if err != nil {
			return nil, err
		}
		output = append(output, event)
	}
	return output, nil
}

// decodeSeqRanges decodes the wire provenance (numbers and [start,end]
// ranges) into the flat seq list.
func decodeSeqRanges(value any, maxEntries int64) ([]any, error) {
	members, ok := value.([]any)
	if !ok {
		return nil, sessionformat.FormatErrorf("sourceEventSeqs must be an array")
	}
	output := []any{}
	hasRange := false
	for _, entry := range members {
		switch typed := entry.(type) {
		case map[string]any:
			return nil, sessionformat.FormatErrorf("sourceEventSeqs range must be a [start, end] pair")
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
			if end < start || end-start+1 > maxEntries-int64(len(output)) {
				return nil, sessionformat.FormatErrorf("sourceEventSeqs range exceeds its event seq")
			}
			for seq := start; seq <= end; seq++ {
				output = append(output, mustDecodeNumber(itoa(seq)))
			}
			hasRange = true
		default:
			if int64(len(output)) >= maxEntries {
				return nil, sessionformat.FormatErrorf("sourceEventSeqs exceeds its event seq")
			}
			seq, err := sessionformat.Count(typed, "sourceEventSeqs member")
			if err != nil {
				return nil, err
			}
			output = append(output, mustDecodeNumber(itoa(seq)))
		}
	}
	if hasRange {
		for index := 1; index < len(output); index++ {
			previous, _ := sessionformat.Count(output[index-1], "sourceEventSeqs member")
			current, _ := sessionformat.Count(output[index], "sourceEventSeqs member")
			if current <= previous {
				return nil, sessionformat.FormatErrorf("sourceEventSeqs ranges must be strictly increasing")
			}
		}
	}
	return output, nil
}

// EncodeArtifact physically encodes one validated artifact in the released
// v0/v1 layout (official encodeArtifact; historical generations only).
func EncodeArtifact(artifact sessionformat.Artifact, options EncodeOptions, version int64) (sessionformat.EncodedArtifact, error) {
	if version == 0 {
		if err := AssertReleasedV0SourceArtifact(artifact); err != nil {
			return sessionformat.EncodedArtifact{}, err
		}
	} else if err := AssertReleasedV1PhysicalArtifact(artifact); err != nil {
		return sessionformat.EncodedArtifact{}, err
	}
	header := artifact.Header
	physicalHeader := map[string]any{
		"type":            "session",
		"version":         mustDecodeNumber(itoa(version)),
		"id":              header["id"],
		"createdAt":       header["createdAt"],
		"delegationDepth": header["delegationDepth"],
	}
	if seeded, _ := header["isSeeded"].(bool); seeded {
		physicalHeader["seedLength"] = mustDecodeNumber(itoa(artifact.InheritedEventCount))
	}
	for _, key := range []string{"cwd", "parentSession", "origin", "agentPreset"} {
		if value, ok := header[key]; ok {
			physicalHeader[key] = value
		}
	}
	headerJSON, err := encodeTree(physicalHeader)
	if err != nil {
		return sessionformat.EncodedArtifact{}, err
	}
	events := make([]any, 0, len(artifact.Events))
	for _, event := range artifact.Events {
		eventRaw, err := json.Marshal(event)
		if err != nil {
			return sessionformat.EncodedArtifact{}, err
		}
		tree, err := sessionformat.DecodeValue(eventRaw)
		if err != nil {
			return sessionformat.EncodedArtifact{}, err
		}
		events = append(events, tree)
	}
	if options.PackChunks {
		events = packChunkRuns(events)
	}
	rows := make([]json.RawMessage, 0, len(events))
	for _, record := range events {
		encoded, err := encodeProvenance(record)
		if err != nil {
			return sessionformat.EncodedArtifact{}, err
		}
		rows = append(rows, encoded)
	}
	return sessionformat.EncodedArtifact{Header: headerJSON, Rows: rows}, nil
}

func encodeProvenance(record any) (json.RawMessage, error) {
	proto, ok := record.(map[string]any)
	if !ok {
		return encodeTree(record)
	}
	sources, hasSources := proto["sourceEventSeqs"]
	if !hasSources {
		return encodeTree(record)
	}
	values, err := decodeSeqRangesList(sources)
	if err != nil {
		return nil, err
	}
	proto["sourceEventSeqs"] = encodeSeqRanges(values)
	return encodeTree(proto)
}

// decodeSeqRangesList re-validates one flat list of seqs into int64 values.
func decodeSeqRangesList(value any) ([]int64, error) {
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

func encodeSeqRanges(values []int64) []any {
	for index := 1; index < len(values); index++ {
		if values[index] <= values[index-1] {
			output := make([]any, 0, len(values))
			for _, value := range values {
				output = append(output, mustDecodeNumber(itoa(value)))
			}
			return output
		}
	}
	output := []any{}
	for start := 0; start < len(values); {
		end := start
		for end+1 < len(values) && values[end+1] == values[end]+1 {
			end++
		}
		if end-start >= 2 {
			output = append(output, []any{mustDecodeNumber(itoa(values[start])), mustDecodeNumber(itoa(values[end]))})
		} else {
			for index := start; index <= end; index++ {
				output = append(output, mustDecodeNumber(itoa(values[index])))
			}
		}
		start = end + 1
	}
	return output
}

type chunkKind string

const (
	textChunkKind      chunkKind = "text-delta"
	reasoningChunkKind chunkKind = "reasoning-delta"
	toolCallChunkKind  chunkKind = "tool-call-delta"
)

// packChunkRuns groups eligible assistant/chunk runs of three or more into
// packed rows (encode-side optimization; decode-side transparent).
func packChunkRuns(events []any) []any {
	output := []any{}
	var kind chunkKind
	var run []any
	flush := func() {
		if kind != "" && len(run) >= 3 {
			output = append(output, buildPackedRow(kind, run))
		} else {
			output = append(output, run...)
		}
		kind = ""
		run = nil
	}
	for _, event := range events {
		candidate, ok := classifyChunk(event)
		if ok && candidate == kind && len(run) > 0 && continuesChunk(run[len(run)-1], event, candidate) {
			run = append(run, event)
			continue
		}
		flush()
		if !ok {
			output = append(output, event)
		} else {
			kind = candidate
			run = []any{event}
		}
	}
	flush()
	return output
}

func classifyChunk(event any) (chunkKind, bool) {
	proto, ok := event.(map[string]any)
	if !ok || proto["type"] != "assistant/chunk" {
		return "", false
	}
	if !hasExactKeys(proto, []string{"type", "seq", "time", "data"}) {
		return "", false
	}
	data, ok := proto["data"].(map[string]any)
	if !ok || !hasExactKeys(data, []string{"turn", "step", "chunk"}) {
		return "", false
	}
	chunk, ok := data["chunk"].(map[string]any)
	if !ok {
		return "", false
	}
	if _, isNumber := chunk["index"].(json.Number); !isNumber {
		return "", false
	}
	chunkType, _ := chunk["type"].(string)
	if chunkType == "text-delta" || chunkType == "reasoning-delta" {
		_, isText := chunk["text"].(string)
		if hasExactKeys(chunk, []string{"type", "index", "text"}) && isText {
			return chunkKind(chunkType), true
		}
		return "", false
	}
	if chunkType != "tool-call-delta" {
		return "", false
	}
	exact := hasExactKeys(chunk, []string{"type", "index", "id", "argumentsDelta"}) ||
		hasExactKeys(chunk, []string{"type", "index", "id", "name", "argumentsDelta"})
	_, idIsString := chunk["id"].(string)
	_, argsIsString := chunk["argumentsDelta"].(string)
	nameOK := true
	if name, exists := chunk["name"]; exists {
		_, nameOK = name.(string)
	}
	return toolCallChunkKind, exact && idIsString && argsIsString && nameOK
}

func continuesChunk(previous, next any, kind chunkKind) bool {
	previousProto, _ := previous.(map[string]any)
	nextProto, _ := next.(map[string]any)
	if previousProto == nil || nextProto == nil {
		return false
	}
	previousData, _ := previousProto["data"].(map[string]any)
	nextData, _ := nextProto["data"].(map[string]any)
	if previousData == nil || nextData == nil {
		return false
	}
	previousChunk, _ := previousData["chunk"].(map[string]any)
	nextChunk, _ := nextData["chunk"].(map[string]any)
	if previousChunk == nil || nextChunk == nil {
		return false
	}
	previousTime, err := sessionformat.SafeInteger(previousProto["time"], "time")
	if err != nil {
		return false
	}
	nextTime, err := sessionformat.SafeInteger(nextProto["time"], "time")
	if err != nil {
		return false
	}
	_ = previousTime
	_ = nextTime
	if previousData["turn"] != nextData["turn"] || previousData["step"] != nextData["step"] {
		return false
	}
	if previousChunk["index"] != nextChunk["index"] {
		return false
	}
	if kind != toolCallChunkKind {
		return true
	}
	_, previousHasName := previousChunk["name"]
	_, nextHasName := nextChunk["name"]
	return previousChunk["id"] == nextChunk["id"] && previousHasName == nextHasName && previousChunk["name"] == nextChunk["name"]
}

func buildPackedRow(kind chunkKind, run []any) map[string]any {
	first, _ := run[0].(map[string]any)
	firstData, _ := first["data"].(map[string]any)
	firstChunk, _ := firstData["chunk"].(map[string]any)
	firstSeq, _ := sessionformat.Count(first["seq"], "seq")
	firstTime, _ := sessionformat.SafeInteger(first["time"], "time")
	dt := make([]any, 0, len(run)-1)
	previousTime := firstTime
	for _, event := range run[1:] {
		proto, _ := event.(map[string]any)
		eventTime, _ := sessionformat.SafeInteger(proto["time"], "time")
		dt = append(dt, mustDecodeNumber(itoa(eventTime-previousTime)))
		previousTime = eventTime
	}
	base := map[string]any{
		"turn":  firstData["turn"],
		"step":  firstData["step"],
		"index": firstChunk["index"],
		"dt":    dt,
	}
	if kind == toolCallChunkKind {
		args := make([]any, 0, len(run))
		for _, event := range run {
			proto, _ := event.(map[string]any)
			data, _ := proto["data"].(map[string]any)
			chunk, _ := data["chunk"].(map[string]any)
			args = append(args, chunk["argumentsDelta"])
		}
		data := map[string]any{}
		for key, value := range base {
			data[key] = value
		}
		data["id"] = firstChunk["id"]
		if name, ok := firstChunk["name"]; ok {
			data["name"] = name
		}
		data["args"] = args
		rowType := "tool-call-chunks"
		return map[string]any{
			"type":  rowType,
			"seq0":  mustDecodeNumber(itoa(firstSeq)),
			"time0": mustDecodeNumber(itoa(firstTime)),
			"data":  data,
		}
	}
	texts := make([]any, 0, len(run))
	for _, event := range run {
		proto, _ := event.(map[string]any)
		data, _ := proto["data"].(map[string]any)
		chunk, _ := data["chunk"].(map[string]any)
		texts = append(texts, chunk["text"])
	}
	data := map[string]any{}
	for key, value := range base {
		data[key] = value
	}
	data["texts"] = texts
	rowType := "text-chunks"
	if kind == reasoningChunkKind {
		rowType = "reasoning-chunks"
	}
	return map[string]any{
		"type":  rowType,
		"seq0":  mustDecodeNumber(itoa(firstSeq)),
		"time0": mustDecodeNumber(itoa(firstTime)),
		"data":  data,
	}
}

func hasExactKeys(record map[string]any, keys []string) bool {
	if len(record) != len(keys) {
		return false
	}
	for _, key := range keys {
		if _, ok := record[key]; !ok {
			return false
		}
	}
	return true
}

// Storage-plane codecs for session logs: lossless seq-range provenance
// encoding and delta-chunk row packing. Port of
// packages/core/session/src/{seq-ranges,chunk-rows}.ts.
package session

import (
	"encoding/json"
	"fmt"
)

// EncodedSeq is one stored provenance entry: a seq or an inclusive
// [start, end] range pair.
type EncodedSeq any

const maxSafeInteger = int64(1)<<53 - 1

func isStrictlyIncreasing(values []int64) bool {
	for i := 1; i < len(values); i++ {
		if values[i] <= values[i-1] {
			return false
		}
	}
	return true
}

// EncodeSeqRanges replaces profitable consecutive runs (three or more) with
// inclusive pairs; any other list stays verbatim. Input order is preserved.
func EncodeSeqRanges(values []int64) []EncodedSeq {
	out := make([]EncodedSeq, 0, len(values))
	if !isStrictlyIncreasing(values) {
		for _, v := range values {
			out = append(out, v)
		}
		return out
	}
	for start := 0; start < len(values); {
		end := start
		for end+1 < len(values) && values[end+1] == values[end]+1 {
			end++
		}
		if end-start >= 2 {
			out = append(out, []int64{values[start], values[end]})
		} else {
			for i := start; i <= end; i++ {
				out = append(out, values[i])
			}
		}
		start = end + 1
	}
	return out
}

// DecodeSeqRanges expands a storage-form provenance array. maxEntries caps
// the expanded length (the owning event's seq): provenance may only cite
// earlier events.
func DecodeSeqRanges(value any, maxEntries int64) ([]int64, error) {
	entries, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("sourceEventSeqs must be an array")
	}
	var decoded []int64
	hasRange := false
	for _, entry := range entries {
		if seq, ok := storageNumberInt(entry); ok {
			if seq < 0 {
				return nil, fmt.Errorf("sourceEventSeqs must contain non-negative safe integers")
			}
			if int64(len(decoded)) >= maxEntries {
				return nil, fmt.Errorf("sourceEventSeqs exceeds its event sequence")
			}
			decoded = append(decoded, seq)
			continue
		}
		var startValue, endValue any
		switch pair := entry.(type) {
		case []any:
			if len(pair) != 2 {
				return nil, fmt.Errorf("sourceEventSeqs range entries must be [start, end] pairs")
			}
			startValue, endValue = pair[0], pair[1]
		case []int64:
			if len(pair) != 2 {
				return nil, fmt.Errorf("sourceEventSeqs range entries must be [start, end] pairs")
			}
			startValue, endValue = pair[0], pair[1]
		default:
			return nil, fmt.Errorf("sourceEventSeqs range entries must be [start, end] pairs")
		}
		start, startOK := storageNumberInt(startValue)
		end, endOK := storageNumberInt(endValue)
		if !startOK || !endOK {
			return nil, fmt.Errorf("sourceEventSeqs range entries must be [start, end] pairs")
		}
		if start < 0 || end < 0 {
			return nil, fmt.Errorf("sourceEventSeqs must contain non-negative safe integers")
		}
		if end < start {
			return nil, fmt.Errorf("sourceEventSeqs ranges require start <= end")
		}
		length := end - start + 1
		if length > maxEntries-int64(len(decoded)) {
			return nil, fmt.Errorf("sourceEventSeqs range exceeds its event sequence")
		}
		for seq := start; seq <= end; seq++ {
			decoded = append(decoded, seq)
		}
		hasRange = true
	}
	if hasRange && !isStrictlyIncreasing(decoded) {
		return nil, fmt.Errorf("sourceEventSeqs ranges must be strictly increasing")
	}
	return decoded, nil
}

// Chunk-row tags: an encoding vocabulary, NOT session events. Bare
// (slash-less) so a reader cannot confuse them with the event taxonomy.
const (
	RowTextChunks      = "text-chunks"
	RowReasoningChunks = "reasoning-chunks"
	RowToolCallChunks  = "tool-call-chunks"
)

// minRun is the run length below which a row's envelope rivals the event
// lines it replaces. A format constant, not a tunable: both layouts decode
// identically.
const minRun = 3

func hasExactKeys(value map[string]any, keys []string) bool {
	if len(value) != len(keys) {
		return false
	}
	for _, key := range keys {
		if _, ok := value[key]; !ok {
			return false
		}
	}
	return true
}

func storageNumberInt(value any) (int64, bool) {
	switch number := value.(type) {
	case json.Number:
		parsed, err := number.Int64()
		if err != nil || parsed > maxSafeInteger || parsed < -maxSafeInteger {
			return 0, false
		}
		return parsed, true
	case int64:
		if number > maxSafeInteger || number < -maxSafeInteger {
			return 0, false
		}
		return number, true
	case float64:
		// Plain json.Unmarshal produces float64; exact integers only.
		if number != float64(int64(number)) || number > float64(maxSafeInteger) || number < float64(-maxSafeInteger) {
			return 0, false
		}
		return int64(number), true
	default:
		return 0, false
	}
}

// classify returns the delta kind when the event's ENTIRE shape (envelope,
// data, chunk — exact keys, primitive types, integer seq/time) is
// whitelisted, else "" (store verbatim). Inputs come from live typed appends
// AND parsed fixture files, so the checks are structural, not type-trusted.
func classify(event Event) string {
	if event.Type != EventAssistantChunk {
		return ""
	}
	var envelope map[string]any
	if err := json.Unmarshal(event.Data, &envelope); err != nil {
		return ""
	}
	if !hasExactKeys(envelope, []string{"turn", "step", "chunk"}) {
		return ""
	}
	if _, ok := storageNumberInt(envelope["turn"]); !ok {
		return ""
	}
	if _, ok := storageNumberInt(envelope["step"]); !ok {
		return ""
	}
	chunk, ok := envelope["chunk"].(map[string]any)
	if !ok {
		return ""
	}
	if _, ok := storageNumberInt(chunk["index"]); !ok {
		return ""
	}
	switch chunk["type"] {
	case "text-delta", "reasoning-delta":
		if hasExactKeys(chunk, []string{"type", "index", "text"}) {
			if _, ok := chunk["text"].(string); ok {
				return chunk["type"].(string)
			}
		}
		return ""
	case "tool-call-delta":
		withName := hasExactKeys(chunk, []string{"type", "index", "id", "name", "argumentsDelta"})
		plain := hasExactKeys(chunk, []string{"type", "index", "id", "argumentsDelta"})
		if !withName && !plain {
			return ""
		}
		if withName {
			if _, ok := chunk["name"].(string); !ok {
				return ""
			}
		}
		if _, ok := chunk["id"].(string); !ok {
			return ""
		}
		if _, ok := chunk["argumentsDelta"].(string); !ok {
			return ""
		}
		return "tool-call-delta"
		// Whitelist fall-through over parsed data: block-start/end, usage,
		// finish, and any future chunk variant stay one event per line.
	default:
		return ""
	}
}

func chunkIndex(event Event) int64 {
	var envelope struct {
		Chunk struct {
			Index int64 `json:"index"`
		} `json:"chunk"`
	}
	_ = json.Unmarshal(event.Data, &envelope)
	return envelope.Chunk.Index
}

func chunkString(event Event, key string) string {
	var envelope struct {
		Chunk map[string]json.RawMessage `json:"chunk"`
	}
	_ = json.Unmarshal(event.Data, &envelope)
	var value string
	_ = json.Unmarshal(envelope.Chunk[key], &value)
	return value
}

// continuesRun reports whether next extends a run ending in prev.
func continuesRun(prev, next Event, kind string) bool {
	if next.Seq != prev.Seq+1 {
		return false
	}
	// Two safe-integer times can sit further apart than an int64... they
	// cannot overflow int64, but the JSON number plane is the double plane:
	// a gap beyond 2^53 would not round-trip exactly.
	gap := next.Time - prev.Time
	if gap > maxSafeInteger || gap < -maxSafeInteger {
		return false
	}
	var prevData, nextData struct {
		Turn int64 `json:"turn"`
		Step int64 `json:"step"`
	}
	_ = json.Unmarshal(prev.Data, &prevData)
	_ = json.Unmarshal(next.Data, &nextData)
	if prevData.Turn != nextData.Turn || prevData.Step != nextData.Step {
		return false
	}
	if chunkIndex(next) != chunkIndex(prev) {
		return false
	}
	if kind != "tool-call-delta" {
		return true
	}
	// `name` must match in presence AND value — a mixed run is not
	// representable.
	prevHasName := decodeChunkHasName(prev.Data)
	nextHasName := decodeChunkHasName(next.Data)
	return chunkString(prev, "id") == chunkString(next, "id") &&
		prevHasName == nextHasName &&
		chunkString(prev, "name") == chunkString(next, "name")
}

func decodeChunkHasName(data json.RawMessage) bool {
	var envelope struct {
		Chunk map[string]json.RawMessage `json:"chunk"`
	}
	_ = json.Unmarshal(data, &envelope)
	_, ok := envelope.Chunk["name"]
	return ok
}

// PackChunkRuns packs an event batch for storage: each run of at least
// minRun consecutive whitelisted same-kind, same-block delta chunk events
// becomes one packed row; every other event passes through verbatim, in
// order. Pure and stateless — safe over any batch, including one whose runs
// were split by flush boundaries (the split runs simply pack per batch).
func PackChunkRuns(events []Event) []map[string]any {
	out := []map[string]any{}
	var kind string
	var run []Event
	flush := func() {
		if kind != "" && len(run) >= minRun {
			out = append(out, buildChunkRow(kind, run))
		} else {
			for _, event := range run {
				out = append(out, eventToMap(event))
			}
		}
		kind = ""
		run = nil
	}
	for _, event := range events {
		k := classify(event)
		if k == "" {
			flush()
			out = append(out, eventToMap(event))
			continue
		}
		if k == kind && len(run) > 0 && continuesRun(run[len(run)-1], event, k) {
			run = append(run, event)
			continue
		}
		flush()
		kind = k
		run = []Event{event}
	}
	flush()
	return out
}

func eventToMap(event Event) map[string]any {
	var decoded map[string]any
	if err := json.Unmarshal(mustMarshalEvent(event), &decoded); err != nil {
		// An Event always marshals and always parses back to an object.
		panic(fmt.Sprintf("session: storage event %q failed to decode: %v", event.Type, err))
	}
	return decoded
}

func mustMarshalEvent(event Event) []byte {
	encoded, err := json.Marshal(event)
	if err != nil {
		panic(fmt.Sprintf("session: storage event %q failed to marshal: %v", event.Type, err))
	}
	return encoded
}

func buildChunkRow(kind string, run []Event) map[string]any {
	first := run[0]
	firstData := map[string]any{}
	_ = json.Unmarshal(first.Data, &firstData)
	turn := firstData["turn"]
	step := firstData["step"]
	index := json.Number(fmt.Sprint(chunkIndex(first)))
	dt := make([]int64, len(run)-1)
	for i := 1; i < len(run); i++ {
		dt[i-1] = run[i].Time - run[i-1].Time
	}
	if kind == "tool-call-delta" {
		data := map[string]any{
			"turn": turn, "step": step, "index": index,
			"id": chunkString(first, "id"), "dt": dt,
		}
		if decodeChunkHasName(first.Data) {
			data["name"] = chunkString(first, "name")
		}
		args := make([]string, len(run))
		for i, event := range run {
			args[i] = chunkString(event, "argumentsDelta")
		}
		data["args"] = args
		return map[string]any{"type": RowToolCallChunks, "seq0": first.Seq, "time0": first.Time, "data": data}
	}
	texts := make([]string, len(run))
	for i, event := range run {
		texts[i] = chunkString(event, "text")
	}
	tag := RowTextChunks
	if kind == "reasoning-delta" {
		tag = RowReasoningChunks
	}
	data := map[string]any{"turn": turn, "step": step, "index": index, "dt": dt, "texts": texts}
	return map[string]any{"type": tag, "seq0": first.Seq, "time0": first.Time, "data": data}
}

func malformedRow(tag, why string) error {
	return fmt.Errorf("malformed %s storage row: %s", tag, why)
}

// stringSlice normalizes a payload list from either type plane: parsed JSON
// ([]any of any) or encoder output ([]string).
func stringSlice(value any) ([]string, bool) {
	switch slice := value.(type) {
	case []any:
		out := make([]string, len(slice))
		for i, entry := range slice {
			s, ok := entry.(string)
			if !ok {
				return nil, false
			}
			out[i] = s
		}
		return out, true
	case []string:
		if len(slice) == 0 {
			return nil, false
		}
		return slice, true
	default:
		return nil, false
	}
}

// intSlice normalizes a gap list from either type plane: parsed JSON
// ([]any of numbers) or encoder output ([]int64).
func intSlice(value any) ([]int64, bool) {
	switch slice := value.(type) {
	case []any:
		out := make([]int64, len(slice))
		for i, entry := range slice {
			n, ok := storageNumberInt(entry)
			if !ok {
				return nil, false
			}
			out[i] = n
		}
		return out, true
	case []int64:
		return slice, true
	default:
		return nil, false
	}
}

// validateRow checks a row-tagged parsed value's envelope and data,
// returning the member payloads and timing anchors.
func validateRow(value map[string]any) (tag string, seq0, time0 int64, data map[string]any, err error) {
	tagRaw, _ := value["type"].(string)
	tag = tagRaw
	if !hasExactKeys(value, []string{"type", "seq0", "time0", "data"}) {
		return tag, 0, 0, nil, malformedRow(tag, "envelope must be exactly {type, seq0, time0, data}")
	}
	seq0, ok := storageNumberInt(value["seq0"])
	if !ok || seq0 < 0 {
		return tag, 0, 0, nil, malformedRow(tag, "seq0 must be a non-negative safe integer")
	}
	time0, ok = storageNumberInt(value["time0"])
	if !ok {
		return tag, 0, 0, nil, malformedRow(tag, "time0 must be a safe integer")
	}
	data, ok = value["data"].(map[string]any)
	if !ok {
		return tag, 0, 0, nil, malformedRow(tag, "data must be an object")
	}
	var payloadKey string
	if tag == RowToolCallChunks {
		withName := hasExactKeys(data, []string{"turn", "step", "index", "id", "name", "dt", "args"})
		plain := hasExactKeys(data, []string{"turn", "step", "index", "id", "dt", "args"})
		if !withName && !plain {
			return tag, 0, 0, nil, malformedRow(tag, "data must be exactly {turn, step, index, id, name?, dt, args}")
		}
		if _, ok := data["id"].(string); !ok {
			return tag, 0, 0, nil, malformedRow(tag, "id (and name when present) must be strings")
		}
		if withName {
			if _, ok := data["name"].(string); !ok {
				return tag, 0, 0, nil, malformedRow(tag, "id (and name when present) must be strings")
			}
		}
		payloadKey = "args"
	} else {
		if !hasExactKeys(data, []string{"turn", "step", "index", "dt", "texts"}) {
			return tag, 0, 0, nil, malformedRow(tag, "data must be exactly {turn, step, index, dt, texts}")
		}
		payloadKey = "texts"
	}
	payload, ok := stringSlice(data[payloadKey])
	if !ok || len(payload) == 0 {
		return tag, 0, 0, nil, malformedRow(tag, payloadKey+" must be a non-empty string array")
	}
	dt, ok := intSlice(data["dt"])
	if !ok {
		return tag, 0, 0, nil, malformedRow(tag, "dt must be an array of safe integers")
	}
	if len(dt) != len(payload)-1 {
		return tag, 0, 0, nil, malformedRow(tag, fmt.Sprintf("dt length %d does not match %d members", len(dt), len(payload)))
	}
	// Reconstruction bounds: a running value that leaves safe range is
	// outside any encoder's image — float arithmetic would round it to a
	// different number than exact arithmetic, a silent corruption.
	if int64(len(payload))-1 > maxSafeInteger-seq0 {
		return tag, 0, 0, nil, malformedRow(tag, "member seqs must stay safe integers")
	}
	time := time0
	for _, gap := range dt {
		time += gap
		if time > maxSafeInteger || time < -maxSafeInteger {
			return tag, 0, 0, nil, malformedRow(tag, "member times must stay safe integers")
		}
	}
	return tag, seq0, time0, data, nil
}

// DecodeStorageRecord decodes one parsed JSONL line value into the session
// events it stores. Row-tagged values validate and expand (a malformed row
// fails loud — it is corrupt storage, and treating it as an event would
// silently drop a whole run); every other value passes through as a single
// event, unvalidated.
func DecodeStorageRecord(value any) ([]Event, error) {
	record, isRecord := value.(map[string]any)
	if !isRecord {
		return nil, fmt.Errorf("stored session records must be objects")
	}
	tag, _ := record["type"].(string)
	if tag != RowTextChunks && tag != RowReasoningChunks && tag != RowToolCallChunks {
		event, err := eventFromParsed(record)
		if err != nil {
			return nil, err
		}
		return []Event{event}, nil
	}
	_, seq0, time0, data, err := validateRow(record)
	if err != nil {
		return nil, err
	}
	turn, _ := storageNumberInt(data["turn"])
	step, _ := storageNumberInt(data["step"])
	index, _ := storageNumberInt(data["index"])
	dt, _ := intSlice(data["dt"])
	payloadKey := "texts"
	if tag == RowToolCallChunks {
		payloadKey = "args"
	}
	members, _ := stringSlice(data[payloadKey])
	events := make([]Event, 0, len(members))
	time := time0
	for k, member := range members {
		if k > 0 {
			time += dt[k-1]
		}
		chunk := map[string]any{"index": index}
		var id, name string
		switch tag {
		case RowTextChunks:
			chunk["type"] = "text-delta"
			chunk["text"] = member
		case RowReasoningChunks:
			chunk["type"] = "reasoning-delta"
			chunk["text"] = member
		case RowToolCallChunks:
			chunk["type"] = "tool-call-delta"
			id, _ = data["id"].(string)
			chunk["id"] = id
			if rawName, has := data["name"]; has {
				name, _ = rawName.(string)
				chunk["name"] = name
			}
			chunk["argumentsDelta"] = member
		}
		chunkJSON, err := json.Marshal(chunk)
		if err != nil {
			return nil, err
		}
		envelope, err := json.Marshal(map[string]any{"turn": turn, "step": step, "chunk": json.RawMessage(chunkJSON)})
		if err != nil {
			return nil, err
		}
		events = append(events, Event{
			Type: EventAssistantChunk,
			Seq:  seq0 + int64(k),
			Time: time,
			Data: envelope,
		})
	}
	return events, nil
}

func eventFromParsed(record map[string]any) (Event, error) {
	raw, err := json.Marshal(record)
	if err != nil {
		return Event{}, err
	}
	var event Event
	if err := json.Unmarshal(raw, &event); err != nil {
		return Event{}, err
	}
	return event, nil
}

// EncodeProvenanceForStorage losslessly shrinks a record's
// sourceEventSeqs for the log: consecutive runs of at least three seqs
// become [start, end] pairs, any other list stays verbatim.
func EncodeProvenanceForStorage(record map[string]any) (map[string]any, error) {
	raw, ok := record["sourceEventSeqs"]
	if !ok {
		return record, nil
	}
	seq, ok := storageNumberInt(record["seq"])
	if !ok || seq < 0 {
		return nil, fmt.Errorf("stored session event seq must be a non-negative safe integer")
	}
	decoded, err := DecodeSeqRanges(raw, seq)
	if err != nil {
		return nil, err
	}
	encoded := EncodeSeqRanges(decoded)
	clone := make(map[string]any, len(record))
	for key, value := range record {
		clone[key] = value
	}
	clone["sourceEventSeqs"] = encoded
	return clone, nil
}

// EventLines serializes an event batch as JSONL lines (no trailing
// newline). With packChunks on, delta-chunk runs pack into storage rows;
// off writes one event per line. Both modes range-encode provenance at the
// storage boundary. Reading is layout-blind either way, so the switch
// changes only newly written bytes. Key order inside each line is Go's map
// marshaling order — JSONL consumers are key-order blind.
func EventLines(events []Event, packChunks bool) ([]byte, error) {
	records := []map[string]any{}
	if packChunks {
		records = PackChunkRuns(events)
	} else {
		for _, event := range events {
			records = append(records, eventToMap(event))
		}
	}
	var out []byte
	for i, record := range records {
		encoded, err := EncodeProvenanceForStorage(record)
		if err != nil {
			return nil, err
		}
		line, err := json.Marshal(encoded)
		if err != nil {
			return nil, err
		}
		if i > 0 {
			out = append(out, '\n')
		}
		out = append(out, line...)
	}
	return out, nil
}

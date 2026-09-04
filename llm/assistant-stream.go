package llm

import (
	"encoding/json"
	"fmt"
)

// Lossless compact representation of one model-stream attempt. Port of
// packages/llm/llm/src/assistant-stream.ts: the encoding owner for the
// Assistant stream embedded in durable format-v2 attempt settlements
// (assistant/message and assistant/attempt). Consecutive text, reasoning, or
// tool-argument deltas for the same block become one compact run with its
// first timestamp, exact timestamp gaps, and one array member per original
// delta; every other chunk remains a timestamped raw record.
// ExpandAssistantStream strictly validates and reconstructs the exact timed
// sequence — compaction never joins delta boundaries.

// LlmAttemptId identifies one model attempt uniquely within one Agent
// lifecycle (official brand: an opaque string; the loop embeds
// `<sessionId>:<counter>`). It must never be confused with provider request
// ids or durable Session ids.
type LlmAttemptId string

// NewLlmAttemptId brands a string as an LlmAttemptId.
func NewLlmAttemptId(id string) LlmAttemptId { return LlmAttemptId(id) }

// TimedStreamChunk is one model chunk paired with its original Session
// timestamp.
type TimedStreamChunk struct {
	Time  int64       `json:"time"`
	Chunk StreamChunk `json:"chunk"`
}

// AssistantStreamRecord is one lossless compact record embedded in a durable
// Assistant attempt event: a sealed union of the three run shapes and the
// timestamped raw chunk. Decoding validates each variant's exact key set.
type AssistantStreamRecord interface {
	isAssistantStreamRecord()
}

// TextRunRecord compacts consecutive text-delta (Type "text-chunks") or
// reasoning-delta (Type "reasoning-chunks") chunks for one block index.
type TextRunRecord struct {
	Type  string `json:"type"`
	Time0 int64  `json:"time0"`
	Index int    `json:"index"`
	// Dt holds the exact timestamp gaps; member i (i > 0) arrives at
	// Time0 + sum(Dt[:i]). Length is always members-1 (an empty array for a
	// single-member run), so the wire always carries the key.
	Dt    []int64  `json:"dt"`
	Texts []string `json:"texts"`
}

// ToolCallRunRecord compacts consecutive tool-call-delta chunks for one
// call identity. Name is present on the wire only when the run's chunks
// carried it (an empty name is not a valid name).
type ToolCallRunRecord struct {
	Type  string     `json:"type"`
	Time0 int64      `json:"time0"`
	Index int        `json:"index"`
	Dt    []int64    `json:"dt"`
	ID    ToolCallID `json:"id"`
	// Name rides the omitempty discipline: present iff the run's first chunk
	// carried a non-empty name.
	Name string   `json:"name,omitempty"`
	Args []string `json:"args"`
}

// RawChunkRecord is any chunk that does not fold into a run, kept verbatim
// with its timestamp (block-start/end, usage, finish, empty-id or empty-name
// tool-call deltas).
type RawChunkRecord struct {
	Type string `json:"type"`
	Time int64  `json:"time"`
	// Chunk is the lossless JSON snapshot of the original StreamChunk.
	Chunk json.RawMessage `json:"chunk"`
}

func (*TextRunRecord) isAssistantStreamRecord()     {}
func (*ToolCallRunRecord) isAssistantStreamRecord() {}
func (*RawChunkRecord) isAssistantStreamRecord()    {}

// run types on the wire.
const (
	RecordTextChunks      = "text-chunks"
	RecordReasoningChunks = "reasoning-chunks"
	RecordToolCallChunks  = "tool-call-chunks"
	RecordRawChunk        = "chunk"
)

// AssistantStreamAccumulator incrementally compacts one attempt without
// retaining a second raw-chunk list.
type AssistantStreamAccumulator struct {
	// records mirrors the official MutableRecord union: runs carry lastTime
	// for gap computation, raw records are frozen as captured.
	records []assistantStreamMutableRecord
}

type assistantStreamMutableRecord struct {
	kind    string // run type or RecordRawChunk
	time0   int64
	index   int
	dt      []int64
	texts   []string
	id      ToolCallID
	name    string
	hasName bool
	args    []string
	// lastTime is run state only; raw records ignore it.
	lastTime int64
	// rawChunk is the captured lossless JSON for raw records.
	rawChunk json.RawMessage
	// rawTime is the captured timestamp for raw records.
	rawTime int64
}

// Push adds one timed chunk to the compact attempt stream. It returns a
// detached copy whose chunk is the lossless JSON snapshot (safe for assembly
// and live publication regardless of later caller mutation).
func (a *AssistantStreamAccumulator) Push(value TimedStreamChunk) (TimedStreamChunk, error) {
	chunkJSON, err := json.Marshal(value.Chunk)
	if err != nil {
		return TimedStreamChunk{}, fmt.Errorf("Assistant stream chunk must be losslessly JSON-serializable: %w", err)
	}
	return a.PushRaw(value.Time, chunkJSON)
}

// PushRaw adds one timed chunk carried as its original lossless JSON bytes.
// Raw chunk records keep those exact bytes, so a durable v1 chunk survives
// a migration round-trip byte-exactly.
func (a *AssistantStreamAccumulator) PushRaw(time int64, chunkJSON []byte) (TimedStreamChunk, error) {
	var snapshot StreamChunk
	if err := json.Unmarshal(chunkJSON, &snapshot); err != nil {
		return TimedStreamChunk{}, fmt.Errorf("Assistant stream chunk must be losslessly JSON-serializable: %w", err)
	}
	timed := TimedStreamChunk{Time: time, Chunk: snapshot}
	switch snapshot.Type {
	case ChunkTextDelta, ChunkReasoningDelta:
		if snapshot.Index < 0 {
			return TimedStreamChunk{}, fmt.Errorf("%s index must be a non-negative safe integer", snapshot.Type)
		}
		runType := RecordTextChunks
		if snapshot.Type == ChunkReasoningDelta {
			runType = RecordReasoningChunks
		}
		if n := len(a.records); n > 0 {
			previous := &a.records[n-1]
			if previous.kind == runType && previous.index == snapshot.Index {
				previous.dt = append(previous.dt, time-previous.lastTime)
				previous.texts = append(previous.texts, snapshot.Text)
				previous.lastTime = time
				return timed, nil
			}
		}
		a.records = append(a.records, assistantStreamMutableRecord{
			kind: runType, time0: time, index: snapshot.Index,
			dt: []int64{}, texts: []string{snapshot.Text}, lastTime: time,
		})
		return timed, nil
	case ChunkToolCallDelta:
		if snapshot.Index < 0 {
			return TimedStreamChunk{}, fmt.Errorf("%s index must be a non-negative safe integer", snapshot.Type)
		}
		// Go's StreamChunk.Name collapses "absent" and "present empty" into
		// one state (documented deviation): the official pushes a
		// present-empty name straight to a raw chunk record, while Go treats
		// it as an absent name eligible for a nameless run.
		if snapshot.ID == "" {
			a.pushRaw(time, chunkJSON)
			return timed, nil
		}
		namePresent := snapshot.Name != ""
		if n := len(a.records); n > 0 {
			previous := &a.records[n-1]
			if previous.kind == RecordToolCallChunks &&
				previous.index == snapshot.Index &&
				previous.id == snapshot.ID &&
				previous.hasName == namePresent && previous.name == snapshot.Name {
				previous.dt = append(previous.dt, time-previous.lastTime)
				previous.args = append(previous.args, snapshot.ArgumentsDelta)
				previous.lastTime = time
				return timed, nil
			}
		}
		record := assistantStreamMutableRecord{
			kind: RecordToolCallChunks, time0: time, index: snapshot.Index,
			dt: []int64{}, id: snapshot.ID, hasName: namePresent, name: snapshot.Name,
			args: []string{snapshot.ArgumentsDelta}, lastTime: time,
		}
		a.records = append(a.records, record)
		return timed, nil
	case ChunkBlockStart, ChunkBlockEnd, ChunkUsage, ChunkFinish:
		a.pushRaw(time, chunkJSON)
		return timed, nil
	default:
		// Unknown chunk kinds stay raw records so a forward extension of the
		// stream protocol survives round-trips losslessly.
		a.pushRaw(time, chunkJSON)
		return timed, nil
	}
}

func (a *AssistantStreamAccumulator) pushRaw(time int64, chunkJSON []byte) {
	captured := append(json.RawMessage{}, chunkJSON...)
	a.records = append(a.records, assistantStreamMutableRecord{
		kind: RecordRawChunk, rawTime: time, rawChunk: captured,
	})
}

// Snapshot returns the current compact attempt stream as detached immutable
// records suitable for a durable event.
func (a *AssistantStreamAccumulator) Snapshot() []AssistantStreamRecord {
	records := make([]AssistantStreamRecord, 0, len(a.records))
	for i := range a.records {
		record := &a.records[i]
		switch record.kind {
		case RecordTextChunks, RecordReasoningChunks:
			records = append(records, &TextRunRecord{
				Type:  record.kind,
				Time0: record.time0,
				Index: record.index,
				Dt:    append([]int64{}, record.dt...),
				Texts: append([]string{}, record.texts...),
			})
		case RecordToolCallChunks:
			run := &ToolCallRunRecord{
				Type:  record.kind,
				Time0: record.time0,
				Index: record.index,
				Dt:    append([]int64{}, record.dt...),
				ID:    record.id,
				Name:  record.name,
				Args:  append([]string{}, record.args...),
			}
			if !record.hasName {
				run.Name = ""
			}
			records = append(records, run)
		default:
			records = append(records, &RawChunkRecord{
				Type:  RecordRawChunk,
				Time:  record.rawTime,
				Chunk: append(json.RawMessage{}, record.rawChunk...),
			})
		}
	}
	return records
}

// ParseAssistantStream validates a durable stream value (the `stream` member
// of a format-v2 attempt settlement) into typed records, enforcing each
// variant's exact key set and member discipline.
func ParseAssistantStream(value json.RawMessage) ([]AssistantStreamRecord, error) {
	if len(value) == 0 {
		return nil, fmt.Errorf("Assistant stream record must be an object")
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(value, &raw); err != nil {
		return nil, fmt.Errorf("Assistant stream must be a record array: %w", err)
	}
	records := make([]AssistantStreamRecord, 0, len(raw))
	for _, member := range raw {
		record, err := parseAssistantStreamRecord(member)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}

func parseAssistantStreamRecord(value json.RawMessage) (AssistantStreamRecord, error) {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(value, &keys); err != nil || keys == nil {
		return nil, fmt.Errorf("Assistant stream record must be an object")
	}
	var envelope struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(value, &envelope)
	switch envelope.Type {
	case RecordTextChunks, RecordReasoningChunks:
		if err := exactRecordKeys(keys, []string{"type", "time0", "index", "dt", "texts"}, envelope.Type); err != nil {
			return nil, err
		}
		var run TextRunRecord
		if err := json.Unmarshal(value, &run); err != nil {
			return nil, fmt.Errorf("%s Assistant stream record: %w", envelope.Type, err)
		}
		if len(run.Texts) == 0 {
			return nil, fmt.Errorf("%s texts must be non-empty", run.Type)
		}
		if err := validateAssistantStreamRun(run.Time0, run.Index, run.Dt, len(run.Texts), run.Type); err != nil {
			return nil, err
		}
		return &run, nil
	case RecordToolCallChunks:
		_, hasName := keys["name"]
		want := []string{"type", "time0", "index", "dt", "id", "args"}
		if hasName {
			want = append(want, "name")
		}
		if err := exactRecordKeys(keys, want, envelope.Type); err != nil {
			return nil, err
		}
		var run ToolCallRunRecord
		if err := json.Unmarshal(value, &run); err != nil {
			return nil, fmt.Errorf("%s Assistant stream record: %w", envelope.Type, err)
		}
		if len(run.Args) == 0 {
			return nil, fmt.Errorf("tool-call-chunks args must be non-empty")
		}
		if run.ID == "" {
			return nil, fmt.Errorf("tool-call-chunks id must be a non-empty string")
		}
		if hasName && run.Name == "" {
			return nil, fmt.Errorf("tool-call-chunks name must be a non-empty string")
		}
		if err := validateAssistantStreamRun(run.Time0, run.Index, run.Dt, len(run.Args), run.Type); err != nil {
			return nil, err
		}
		return &run, nil
	case RecordRawChunk:
		if err := exactRecordKeys(keys, []string{"type", "time", "chunk"}, RecordRawChunk); err != nil {
			return nil, err
		}
		var raw RawChunkRecord
		if err := json.Unmarshal(value, &raw); err != nil {
			return nil, fmt.Errorf("%s Assistant stream record: %w", RecordRawChunk, err)
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw.Chunk, &object); err != nil || object == nil {
			return nil, fmt.Errorf("Assistant stream raw chunk must be a lossless JSON object")
		}
		return &raw, nil
	default:
		return nil, fmt.Errorf("Unsupported Assistant stream record %q", envelope.Type)
	}
}

func exactRecordKeys(got map[string]json.RawMessage, want []string, label string) error {
	if len(got) != len(want) {
		return fmt.Errorf("%s Assistant stream record must contain exactly %v", label, want)
	}
	for _, key := range want {
		if _, ok := got[key]; !ok {
			return fmt.Errorf("%s Assistant stream record must contain exactly %v", label, want)
		}
	}
	return nil
}

func validateAssistantStreamRun(time0 int64, index int, dt []int64, members int, label string) error {
	if index < 0 {
		return fmt.Errorf("%s index must be a non-negative safe integer", label)
	}
	if len(dt) != members-1 {
		return fmt.Errorf("%s dt length must be one less than its members", label)
	}
	time := time0
	for _, gap := range dt {
		time += gap
		if time < -1<<62 || time > 1<<62 {
			return fmt.Errorf("%s member times must stay safe integers", label)
		}
	}
	return nil
}

// ExpandAssistantStream expands compact records into the exact timed chunk
// sequence, preserving every original delta boundary.
func ExpandAssistantStream(stream []AssistantStreamRecord) ([]TimedStreamChunk, error) {
	chunks := make([]TimedStreamChunk, 0)
	for _, candidate := range stream {
		switch record := candidate.(type) {
		case *RawChunkRecord:
			var chunk StreamChunk
			if err := json.Unmarshal(record.Chunk, &chunk); err != nil {
				return nil, fmt.Errorf("Assistant stream raw chunk: %w", err)
			}
			chunks = append(chunks, TimedStreamChunk{Time: record.Time, Chunk: chunk})
		case *TextRunRecord:
			time := record.Time0
			for i, text := range record.Texts {
				if i > 0 {
					time += record.Dt[i-1]
				}
				chunkType := ChunkTextDelta
				if record.Type == RecordReasoningChunks {
					chunkType = ChunkReasoningDelta
				}
				chunks = append(chunks, TimedStreamChunk{Time: time, Chunk: StreamChunk{
					Type: chunkType, Index: record.Index, Text: text,
				}})
			}
		case *ToolCallRunRecord:
			time := record.Time0
			for i, args := range record.Args {
				if i > 0 {
					time += record.Dt[i-1]
				}
				chunk := StreamChunk{
					Type: ChunkToolCallDelta, Index: record.Index, ID: record.ID,
					ArgumentsDelta: args,
				}
				if record.Name != "" {
					chunk.Name = record.Name
				}
				chunks = append(chunks, TimedStreamChunk{Time: time, Chunk: chunk})
			}
		default:
			return nil, fmt.Errorf("Unsupported Assistant stream record %T", candidate)
		}
	}
	return chunks, nil
}

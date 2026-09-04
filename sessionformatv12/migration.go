package sessionformatv12

import (
	"encoding/json"

	"dshgo/llm"
	"dshgo/sessionformat"
	"dshgo/sessionformatv01"
)

// Adjacent migration that embeds released-v1 top-level Assistant chunks
// into v2 attempt events. Port of
// packages/session/session-format-v1-to-v2/src/migration.ts.

// MigrationName is the edge's stable identity.
const MigrationName = "@deepseek-ai/dsh-session-format-v1-to-v2"

// Migration is the compiled v1 -> v2 edge.
type Migration struct{}

// NewMigration returns the adjacent v1 -> v2 migration.
func NewMigration() Migration { return Migration{} }

// Name implements sessionformat.Migration.
func (Migration) Name() string { return MigrationName }

// FromVersion implements sessionformat.Migration.
func (Migration) FromVersion() int64 { return 1 }

// ToVersion implements sessionformat.Migration.
func (Migration) ToVersion() int64 { return 2 }

// MigrateHeader implements sessionformat.Migration.
func (Migration) MigrateHeader(header sessionformat.Header) (sessionformat.Header, error) {
	if err := sessionformatv01.AssertReleasedV1Header(header); err != nil {
		return nil, err
	}
	next := header.Clone()
	next["version"] = numberOf(2)
	return next, nil
}

// Migrate implements sessionformat.Migration.
func (m Migration) Migrate(source sessionformat.Artifact) (sessionformat.Artifact, error) {
	if err := sessionformatv01.AssertReleasedV1Artifact(source); err != nil {
		return sessionformat.Artifact{}, err
	}
	for _, event := range source.Events {
		if !sessionformatv01.HasDisposition(event.Type) {
			return sessionformat.Artifact{}, refusalf(
				"format v1 contains unknown event type %q at seq %d", event.Type, event.Seq)
		}
	}
	groups, err := collectAttemptGroups(source.Events)
	if err != nil {
		return sessionformat.Artifact{}, err
	}
	groupByChunk := map[int64]*attemptGroup{}
	groupByMessage := map[int64]*attemptGroup{}
	for _, group := range groups {
		for _, chunk := range group.chunks {
			groupByChunk[chunk.Seq] = group
		}
		if group.messageSeq != nil {
			groupByMessage[*group.messageSeq] = group
		}
	}

	staged := []stagedEvent{}
	oldToNew := map[int64]int64{}
	stage := func(origin int64, event sessionformat.Event) {
		oldToNew[origin] = int64(len(staged))
		staged = append(staged, stagedEvent{origin: origin, event: event})
	}
	for _, sourceEvent := range source.Events {
		if group, ok := groupByChunk[sourceEvent.Seq]; ok {
			if group.messageSeq == nil && sourceEvent.Seq == group.chunks[len(group.chunks)-1].Seq {
				attempt, err := attemptEvent(group)
				if err != nil {
					return sessionformat.Artifact{}, err
				}
				stage(sourceEvent.Seq, attempt)
			}
			continue
		}
		if group, ok := groupByMessage[sourceEvent.Seq]; ok {
			message, err := messageEvent(sourceEvent, group)
			if err != nil {
				return sessionformat.Artifact{}, err
			}
			stage(sourceEvent.Seq, message)
			continue
		}
		event := sourceEvent
		seeded, _ := source.Header["isSeeded"].(bool)
		if seeded && sourceEvent.Seq == source.InheritedEventCount && sourceEvent.Type == "session/end-seed" {
			data := map[string]any{"inherited": true}
			event.Data = mustJSON(data)
		}
		stage(sourceEvent.Seq, event)
	}

	inheritedEventCount, err := remapInheritedCut(source, groups, staged)
	if err != nil {
		return sessionformat.Artifact{}, err
	}
	seeded, _ := source.Header["isSeeded"].(bool)
	if seeded {
		nextIsEndSeed := source.InheritedEventCount < int64(len(source.Events)) &&
			source.Events[source.InheritedEventCount].Type == "session/end-seed"
		if !nextIsEndSeed {
			var next, previous *sessionformat.Event
			if source.InheritedEventCount < int64(len(source.Events)) {
				next = &source.Events[source.InheritedEventCount]
			}
			if source.InheritedEventCount > 0 {
				previous = &source.Events[source.InheritedEventCount-1]
			}
			time := headerCreatedAt(source.Header)
			if next != nil {
				time = next.Time
			} else if previous != nil {
				time = previous.Time
			}
			marker := sessionformat.Event{
				Type: "session/end-seed",
				Seq:  inheritedEventCount,
				Time: time,
				Data: mustJSON(map[string]any{"inherited": true}),
			}
			staged = append(staged, stagedEvent{})
			copy(staged[inheritedEventCount+1:], staged[inheritedEventCount:])
			staged[inheritedEventCount] = stagedEvent{origin: -1, event: marker}
			oldToNew = map[int64]int64{}
			for index, candidate := range staged {
				if candidate.origin >= 0 {
					oldToNew[candidate.origin] = int64(index)
				}
			}
		}
	}
	for _, group := range groups {
		for _, chunk := range group.chunks {
			delete(oldToNew, chunk.Seq)
		}
	}

	target := sessionformat.Artifact{
		Header:              source.Header.Clone(),
		InheritedEventCount: inheritedEventCount,
		Events:              make([]sessionformat.Event, 0, len(staged)),
	}
	target.Header["version"] = numberOf(2)
	for index, candidate := range staged {
		remapped, err := remapReferences(candidate.event, int64(index), oldToNew)
		if err != nil {
			return sessionformat.Artifact{}, err
		}
		target.Events = append(target.Events, remapped)
	}
	if _, err := sessionformat.SnapshotArtifact(target, "released v1-to-v2 target"); err != nil {
		return sessionformat.Artifact{}, err
	}
	if err := AssertReleasedV2Artifact(target); err != nil {
		return sessionformat.Artifact{}, err
	}
	return target, nil
}

type stagedEvent struct {
	origin int64
	event  sessionformat.Event
}

// ValidateTarget implements sessionformat.Migration.
func (Migration) ValidateTarget(artifact sessionformat.Artifact) error {
	return AssertReleasedV2Artifact(artifact)
}

// ValidateTargetHeader implements sessionformat.Migration.
func (Migration) ValidateTargetHeader(header sessionformat.Header) error {
	return AssertReleasedV2Header(header)
}

// headerCreatedAt is a small accessor for the seed-marker timestamp.
func headerCreatedAt(header sessionformat.Header) int64 {
	value, _ := header["createdAt"].(json.Number)
	parsed, _ := value.Int64()
	return parsed
}

type attemptGroup struct {
	turn     int64
	step     int64
	chunks   []sessionformat.Event
	terminal bool
	// messageSeq is the owning assistant/message position, when claimed.
	messageSeq *int64
}

func refusalf(format string, args ...any) error {
	return sessionformat.UnsupportedErrorf(format, args...)
}

func collectAttemptGroups(events []sessionformat.Event) ([]*attemptGroup, error) {
	groups := []*attemptGroup{}
	current := map[string]*attemptGroup{}
	for _, event := range events {
		if event.Type == "assistant/chunk" {
			data, err := decodeData(event)
			if err != nil {
				return nil, err
			}
			turn, err := coordinate(data["turn"])
			if err != nil {
				return nil, err
			}
			step, err := coordinate(data["step"])
			if err != nil {
				return nil, err
			}
			key := itoa(turn) + ":" + itoa(step)
			group := current[key]
			if group == nil || group.terminal {
				group = &attemptGroup{turn: turn, step: step}
				groups = append(groups, group)
				current[key] = group
			}
			group.chunks = append(group.chunks, event)
			chunk, err := recordOf(data["chunk"], "chunk")
			if err != nil {
				return nil, err
			}
			if chunk["type"] == "finish" {
				group.terminal = true
			}
			continue
		}
		if event.Type != "assistant/message" {
			closeAttemptAtBoundary(event, current)
			continue
		}
		data, err := decodeData(event)
		if err != nil {
			return nil, err
		}
		turn, err := coordinate(data["turn"])
		if err != nil {
			return nil, err
		}
		step, err := coordinate(data["step"])
		if err != nil {
			return nil, err
		}
		sourcesRaw, hasSources := event.ExtraField("sourceEventSeqs")
		if !hasSources {
			unclaimed := false
			for _, candidate := range groups {
				if candidate.messageSeq == nil && candidate.turn == turn && candidate.step == step {
					unclaimed = true
					break
				}
			}
			if unclaimed {
				return nil, refusalf(
					"assistant/message %d does not cite its complete v1 chunk attempt", event.Seq)
			}
			seq := event.Seq
			groups = append(groups, &attemptGroup{turn: turn, step: step, terminal: true, messageSeq: &seq})
			continue
		}
		sources, err := decodeSeqList(sourcesRaw)
		if err != nil {
			return nil, err
		}
		if len(sources) == 0 {
			// Released v1 uses an explicit empty list to state that this
			// message owns no preceding chunks; an absent list cannot make
			// that claim.
			seq := event.Seq
			groups = append(groups, &attemptGroup{turn: turn, step: step, terminal: true, messageSeq: &seq})
			continue
		}
		var matched *attemptGroup
		for _, candidate := range groups {
			if candidate.messageSeq != nil || candidate.turn != turn || candidate.step != step {
				continue
			}
			if sameNumbers(chunkSeqs(candidate.chunks), sources) {
				matched = candidate
				break
			}
		}
		if matched == nil {
			return nil, refusalf(
				"assistant/message %d chunk provenance is not one complete ordered attempt", event.Seq)
		}
		seq := event.Seq
		matched.messageSeq = &seq
		matched.terminal = true
	}
	return groups, nil
}

func closeAttemptAtBoundary(event sessionformat.Event, current map[string]*attemptGroup) {
	if event.Type == "turn/end" {
		data, err := decodeData(event)
		if err != nil {
			return
		}
		turn, err := coordinate(data["turn"])
		if err != nil {
			return
		}
		for _, group := range current {
			if group.turn == turn {
				group.terminal = true
			}
		}
		return
	}
	if event.Type != "step/end" && event.Type != "llm/retry" && event.Type != "llm/retry-started" {
		return
	}
	data, err := decodeData(event)
	if err != nil {
		return
	}
	turn, err := coordinate(data["turn"])
	if err != nil {
		return
	}
	step, err := coordinate(data["step"])
	if err != nil {
		return
	}
	if group, ok := current[itoa(turn)+":"+itoa(step)]; ok {
		group.terminal = true
	}
}

// streamOf compacts one attempt's chunks through the runtime accumulator
// (the v2 stream encoding owner).
func streamOf(group *attemptGroup) ([]llm.AssistantStreamRecord, error) {
	accumulator := &llm.AssistantStreamAccumulator{}
	for _, event := range group.chunks {
		data, err := decodeData(event)
		if err != nil {
			return nil, err
		}
		chunk, err := recordOf(data["chunk"], "chunk")
		if err != nil {
			return nil, err
		}
		chunkJSON, err := sessionformat.EncodeValue(chunk)
		if err != nil {
			return nil, err
		}
		if _, err := accumulator.PushRaw(event.Time, chunkJSON); err != nil {
			return nil, err
		}
	}
	return accumulator.Snapshot(), nil
}

func messageEvent(source sessionformat.Event, group *attemptGroup) (sessionformat.Event, error) {
	data, err := decodeData(source)
	if err != nil {
		return source, err
	}
	stream, err := streamOf(group)
	if err != nil {
		return source, err
	}
	data["stream"] = streamRecordsToAny(stream)
	event := source
	deleteEnvelopeExtra(&event, "sourceEventSeqs")
	return setData(event, data)
}

func attemptEvent(group *attemptGroup) (sessionformat.Event, error) {
	last := group.chunks[len(group.chunks)-1]
	stream, err := streamOf(group)
	if err != nil {
		return sessionformat.Event{}, err
	}
	data := map[string]any{
		"turn":   numberOf(group.turn),
		"step":   numberOf(group.step),
		"stream": streamRecordsToAny(stream),
	}
	return sessionformat.Event{
		Type: "assistant/attempt",
		Seq:  last.Seq,
		Time: last.Time,
		Data: mustJSON(data),
	}, nil
}

// streamRecordsToAny renders compact records as a JSON tree member.
func streamRecordsToAny(records []llm.AssistantStreamRecord) any {
	encoded, err := json.Marshal(records)
	if err != nil {
		panic(err)
	}
	return mustDecode(encoded)
}

func remapInheritedCut(source sessionformat.Artifact, groups []*attemptGroup, staged []stagedEvent) (int64, error) {
	cut := source.InheritedEventCount
	for _, group := range groups {
		members := chunkSeqs(group.chunks)
		if group.messageSeq != nil {
			members = append(members, *group.messageSeq)
		}
		before, after := false, false
		for _, seq := range members {
			if seq < cut {
				before = true
			} else {
				after = true
			}
		}
		if before && after {
			return 0, refusalf("inherited Session cut %d splits one Assistant attempt", cut)
		}
	}
	count := int64(0)
	for _, candidate := range staged {
		if candidate.origin < cut {
			count++
		}
	}
	return count, nil
}

func remapReferences(source sessionformat.Event, targetSeq int64, mapping map[int64]int64) (sessionformat.Event, error) {
	event := source
	sourcesRaw, hadSources := event.ExtraField("sourceEventSeqs")
	operationRaw, hadOperation := event.ExtraField("surfaceOp")
	deleteEnvelopeExtra(&event, "sourceEventSeqs")
	deleteEnvelopeExtra(&event, "surfaceOp")
	event.Seq = targetSeq
	data, err := decodeData(event)
	if err != nil {
		return event, err
	}
	remapped, err := remapPayloadReferences(event, data, mapping)
	if err != nil {
		return event, err
	}
	event, err = setData(event, remapped)
	if err != nil {
		return event, err
	}
	if hadSources {
		sources, err := decodeSeqList(sourcesRaw)
		if err != nil {
			return event, err
		}
		mapped := make([]int64, 0, len(sources))
		for _, value := range sources {
			target, err := mapOne(value, mapping, event.Type+" "+itoa(source.Seq)+" sources")
			if err != nil {
				return event, err
			}
			mapped = append(mapped, target)
		}
		event = setEnvelopeExtra(event, "sourceEventSeqs", mustJSON(int64ListToAny(mapped)))
	}
	if hadOperation {
		if string(operationRaw) != `"append"` {
			tree, err := sessionformat.DecodeValue(operationRaw)
			if err != nil {
				return event, err
			}
			replacement, ok := tree.(map[string]any)
			if !ok {
				return event, sessionformat.FormatErrorf("surfaceOp must be an object")
			}
			start, err := coordinate(replacement["start"])
			if err != nil {
				return event, err
			}
			end, err := coordinate(replacement["end"])
			if err != nil {
				return event, err
			}
			mappedStart, err := mapOne(start, mapping, event.Type+" "+itoa(source.Seq)+" surface start")
			if err != nil {
				return event, err
			}
			mappedEnd, err := mapOne(end, mapping, event.Type+" "+itoa(source.Seq)+" surface end")
			if err != nil {
				return event, err
			}
			operation := map[string]any{
				"op":    "replace",
				"start": numberOf(mappedStart),
				"end":   numberOf(mappedEnd),
			}
			event = setEnvelopeExtra(event, "surfaceOp", mustJSON(operation))
		} else {
			event = setEnvelopeExtra(event, "surfaceOp", operationRaw)
		}
	}
	return event, nil
}

func remapPayloadReferences(event sessionformat.Event, data map[string]any, mapping map[int64]int64) (map[string]any, error) {
	switch event.Type {
	case "command/done":
		if value, ok := data["sourceEventSeq"]; ok {
			seq, err := coordinate(value)
			if err != nil {
				return nil, err
			}
			target, err := mapOne(seq, mapping, "command/done "+itoa(event.Seq)+" sourceEventSeq")
			if err != nil {
				return nil, err
			}
			data["sourceEventSeq"] = numberOf(target)
		}
		return data, nil
	case "compaction/prune", "compaction/summary":
		rangeRecord, ok := data["shadowedRange"].(map[string]any)
		if !ok {
			return nil, sessionformat.FormatErrorf("%s %d shadowedRange must be an object", event.Type, event.Seq)
		}
		start, err := coordinate(rangeRecord["start"])
		if err != nil {
			return nil, err
		}
		end, err := coordinate(rangeRecord["end"])
		if err != nil {
			return nil, err
		}
		mappedStart, err := mapOne(start, mapping, event.Type+" "+itoa(event.Seq)+" shadowedRange start")
		if err != nil {
			return nil, err
		}
		mappedEnd, err := mapOne(end, mapping, event.Type+" "+itoa(event.Seq)+" shadowedRange end")
		if err != nil {
			return nil, err
		}
		data["shadowedRange"] = map[string]any{"start": numberOf(mappedStart), "end": numberOf(mappedEnd)}
		seqs, err := decodeSeqList(mustJSON(data["shadowedSeqs"]))
		if err != nil {
			return nil, err
		}
		mapped := make([]int64, 0, len(seqs))
		for _, value := range seqs {
			target, err := mapOne(value, mapping, event.Type+" "+itoa(event.Seq)+" shadowedSeqs")
			if err != nil {
				return nil, err
			}
			mapped = append(mapped, target)
		}
		data["shadowedSeqs"] = int64ListToAny(mapped)
		return data, nil
	case "session/title", "session/title-llm-request":
		seqs, err := decodeSeqList(mustJSON(data["messageSeqs"]))
		if err != nil {
			return nil, err
		}
		mapped := make([]int64, 0, len(seqs))
		for _, value := range seqs {
			target, err := mapOne(value, mapping, event.Type+" "+itoa(event.Seq)+" messageSeqs")
			if err != nil {
				return nil, err
			}
			mapped = append(mapped, target)
		}
		data["messageSeqs"] = int64ListToAny(mapped)
		return data, nil
	default:
		return data, nil
	}
}

func mapOne(value int64, mapping map[int64]int64, label string) (int64, error) {
	mapped, ok := mapping[value]
	if !ok {
		return 0, refusalf("%s targets consumed assistant/chunk %d", label, value)
	}
	return mapped, nil
}

func chunkSeqs(chunks []sessionformat.Event) []int64 {
	seqs := make([]int64, 0, len(chunks))
	for _, chunk := range chunks {
		seqs = append(seqs, chunk.Seq)
	}
	return seqs
}

func decodeSeqList(raw json.RawMessage) ([]int64, error) {
	tree, err := sessionformat.DecodeValue(raw)
	if err != nil {
		return nil, err
	}
	members, ok := tree.([]any)
	if !ok {
		return nil, sessionformat.FormatErrorf("expected a seq array")
	}
	output := make([]int64, 0, len(members))
	for _, member := range members {
		seq, err := sessionformat.Count(member, "seq member")
		if err != nil {
			return nil, err
		}
		output = append(output, seq)
	}
	return output, nil
}

func int64ListToAny(values []int64) any {
	members := make([]any, 0, len(values))
	for _, value := range values {
		members = append(members, numberOf(value))
	}
	return members
}

func sameNumbers(left, right []int64) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func coordinate(value any) (int64, error) {
	return sessionformat.Count(value, "coordinate")
}

func recordOf(value any, label string) (map[string]any, error) {
	record, ok := value.(map[string]any)
	if !ok {
		return nil, sessionformat.FormatErrorf("%s must be a JSON object", label)
	}
	return record, nil
}

func setData(event sessionformat.Event, data map[string]any) (sessionformat.Event, error) {
	raw, err := sessionformat.EncodeValue(data)
	if err != nil {
		return event, err
	}
	event.Data = raw
	return event, nil
}

func deleteEnvelopeExtra(event *sessionformat.Event, key string) {
	if event.Extra != nil {
		delete(event.Extra, key)
	}
}

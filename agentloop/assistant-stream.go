package agentloop

import (
	"encoding/json"

	"dshgo/agent"
	"dshgo/llm"
	"dshgo/session"
)

// The frame vocabulary (agent.AssistantStreamFrame/agent.AssistantStreamOutcome) lives
// in the agent package next to the event bus it rides.

// Process-local assistant attempt framing and durable stream accumulation.
// Port of packages/core/agent-loop/src/assistant-stream.ts: one model
// attempt folds into one compact stream plus ordered transient
// `agent/assistant-stream` frames. Durable settlements remain the source of
// replay and model history; the frames are intentionally process-local
// presentation (a restart has no active attempts).

// AssistantStreamAttempt folds one model attempt into one compact stream
// plus its transient frames.
type AssistantStreamAttempt struct {
	accumulator *llm.AssistantStreamAccumulator
	assembler   *llm.BlockAssembler
	index       int64
	terminal    bool
	emit        func(agent.AssistantStreamFrame)
	// AttemptID is unique within this Agent lifecycle.
	AttemptID llm.LlmAttemptId
	Turn      int64
	Step      int64
}

// newAssistantStreamAttempt opens one attempt. attempt is the attached-
// Session-local counter; nextRevision allocates the next emitted frame
// revision.
func newAssistantStreamAttempt(
	sessionID session.SessionID,
	attempt int64,
	nextRevision func() int64,
	turn, step int64,
	emit func(agent.AssistantStreamFrame),
) *AssistantStreamAttempt {
	return &AssistantStreamAttempt{
		accumulator: &llm.AssistantStreamAccumulator{},
		assembler:   llm.NewBlockAssembler(),
		emit:        emit,
		AttemptID:   llm.NewLlmAttemptId(string(sessionID) + ":" + int64ToString(attempt)),
		Turn:        turn,
		Step:        step,
	}
}

func int64ToString(value int64) string {
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

// Ended reports whether this started attempt has emitted its terminal frame.
func (a *AssistantStreamAttempt) Ended() bool { return a.terminal }

// Start publishes the opening marker before the first delivered chunk.
func (a *AssistantStreamAttempt) Start(revision int64) {
	a.emit(agent.AssistantStreamFrame{
		Type: "start", AttemptID: a.AttemptID, Revision: revision,
		Turn: a.Turn, Step: a.Step,
	})
}

// Push snapshots one chunk once, then feeds durable compaction, assembly,
// and live publication. The chunk rides as its lossless JSON so the raw
// record keeps the exact bytes.
func (a *AssistantStreamAttempt) Push(timed llm.TimedStreamChunk, revision int64, chunkJSON []byte) {
	chunk := timed.Chunk
	_, _ = a.accumulator.PushRaw(timed.Time, chunkJSON)
	a.assembler.Push(chunk)
	a.emit(agent.AssistantStreamFrame{
		Type: "chunk", AttemptID: a.AttemptID, Revision: revision,
		Turn: a.Turn, Step: a.Step,
		Index: a.index, Time: timed.Time, Chunk: &chunk,
	})
	a.index++
}

// Settle publishes the terminal settlement after the matching durable event
// commits. An append failure abandons the attempt and re-raises.
func (a *AssistantStreamAttempt) Settle(eventType string, revision int64, append func() (session.Event, error)) (session.Event, error) {
	committed, err := append()
	if err != nil {
		a.Abandon(revision)
		return session.Event{}, err
	}
	a.terminal = true
	a.emit(agent.AssistantStreamFrame{
		Type: "end", AttemptID: a.AttemptID, Revision: revision,
		Turn: a.Turn, Step: a.Step,
		Index: a.index,
		Outcome: &agent.AssistantStreamOutcome{
			Kind: "committed", EventType: eventType, Seq: committed.Seq,
		},
	})
	return committed, nil
}

// Abandon publishes abandonment when no durable attempt event can be
// committed.
func (a *AssistantStreamAttempt) Abandon(revision int64) {
	a.terminal = true
	a.emit(agent.AssistantStreamFrame{
		Type: "end", AttemptID: a.AttemptID, Revision: revision,
		Turn: a.Turn, Step: a.Step,
		Index: a.index,
		Outcome: &agent.AssistantStreamOutcome{Kind: "abandoned"},
	})
}

// StreamJSON renders the exact compact stream for the final durable event.
func (a *AssistantStreamAttempt) StreamJSON() (json.RawMessage, error) {
	records := a.accumulator.Snapshot()
	encoded, err := json.Marshal(records)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

// Blocks returns the canonical completed-message blocks from the same
// chunks.
func (a *AssistantStreamAttempt) Blocks() []llm.ContentBlock { return a.assembler.Blocks() }

// InterruptedBlocks returns the safe visible prefix when cancellation
// interrupts the attempt.
func (a *AssistantStreamAttempt) InterruptedBlocks() []llm.ContentBlock {
	return a.assembler.InterruptedBlocks()
}

// Usage returns the latest adapter-reported usage in the stream.
func (a *AssistantStreamAttempt) Usage() *llm.TokenUsage { return a.assembler.Usage() }

// Finish returns the terminal reason, defaulting to stop when the stream
// omitted one.
func (a *AssistantStreamAttempt) Finish() llm.FinishReason { return a.assembler.Finish() }

// ReplayState returns the replay metadata carried by the terminal finish
// record.
func (a *AssistantStreamAttempt) ReplayState() *llm.ReplayEnvelope { return a.assembler.ReplayState() }

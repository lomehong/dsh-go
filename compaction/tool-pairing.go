package compaction

import (
	"encoding/json"
	"fmt"

	"dshgo/llm"
	"dshgo/session"
)

// ToolPairingBalance tracks tool-pairing balance over one session surface.
// Compaction changes surface positions, so safe cuts are derived from
// tool-call/result content in current surface order rather than step
// markers.
//
// Go adaptation: the source reaches per-session state through a WeakMap;
// Go has no weak references, so the cache is an OWNED value instead —
// whichever component drives a session (the compaction backend) holds one
// balance for its lifetime. The generation check still detects surface
// rewrites, so a shared instance remains correct across replaces.
type ToolPairingBalance struct {
	// owner is the session this state describes; a different session forces
	// a rebuild (the source's WeakMap keyed per session; Go has no weak
	// references, so the owned instance detects the switch itself).
	owner *session.Session
	// generation is the surface rewrite generation this state describes.
	generation int64
	// cutBalanced is the balance of every surface cut in current order: a
	// surface of N sequences has N+1 cuts, entry i being the cut before
	// sequence i and the final entry the cut after the surface tail.
	cutBalanced []bool
	// indexBySeq is the current surface position of each event seq,
	// indexing cutBalanced.
	indexBySeq map[int64]int
	// inProgressToolCalls is the in-progress tool-call count after the
	// processed surface tail.
	inProgressToolCalls int
}

// NewToolPairingBalance builds the empty-surface state whose single leading
// cut is trivially balanced.
func NewToolPairingBalance() *ToolPairingBalance {
	return &ToolPairingBalance{
		generation:  -1,
		cutBalanced: []bool{true},
		indexBySeq:  map[int64]int{},
	}
}

// eventDelta returns how one surface event changes the in-progress
// tool-call count.
func eventDelta(event session.Event) int {
	switch event.Type {
	case session.EventAssistantMsg:
		decoded, err := session.DecodeAssistantMessage(event)
		if err != nil {
			return 0
		}
		count := 0
		for _, block := range decoded.Message.Content {
			if block.Type == llm.BlockToolCall {
				count++
			}
		}
		return count
	case session.EventToolResult:
		return -1
	default:
		return 0
	}
}

// eventForSeq reads and validates the event named by a surface sequence.
func eventForSeq(events []session.Event, seq int64) (session.Event, error) {
	if seq < 0 || int(seq) >= len(events) {
		return session.Event{}, fmt.Errorf("tool-pairing balance: surface seq %d has no matching session event (corrupt surface)", seq)
	}
	event := events[seq]
	if event.Seq != seq {
		return session.Event{}, fmt.Errorf("tool-pairing balance: surface seq %d has no matching session event (corrupt surface)", seq)
	}
	return event, nil
}

// synchronize folds surface sequences not yet in the cache into the balance
// state. The unseen tail is validated before mutating the live state, so a
// corrupt append cannot leave a partially advanced state behind.
func (b *ToolPairingBalance) synchronize(sess *session.Session) error {
	surface := sess.Surface()
	seqs := surface.Nodes()
	generation := surface.ReplaceGeneration()
	if b.owner == sess && b.generation == generation && len(b.cutBalanced)-1 >= len(seqs) {
		return nil
	}
	if b.owner != sess || b.generation != generation || len(b.cutBalanced)-1 > len(seqs) {
		// A rebuild is the same fold started from the empty-surface state.
		b.owner = sess
		b.generation = generation
		b.cutBalanced = []bool{true}
		b.indexBySeq = map[int64]int{}
		b.inProgressToolCalls = 0
	}
	events := sess.Events()
	processed := len(b.cutBalanced) - 1
	pendingCuts := make([]bool, 0, len(seqs)-processed)
	for _, seq := range seqs[processed:] {
		event, err := eventForSeq(events, seq)
		if err != nil {
			return err
		}
		b.inProgressToolCalls += eventDelta(event)
		if b.inProgressToolCalls < 0 {
			return fmt.Errorf("tool-pairing balance: tool/result at surface seq %d has no matching tool-call (corrupt surface)", seq)
		}
		pendingCuts = append(pendingCuts, b.inProgressToolCalls == 0)
	}
	for offset, seq := range seqs[processed:] {
		b.indexBySeq[seq] = processed + offset
	}
	b.cutBalanced = append(b.cutBalanced, pendingCuts...)
	return nil
}

// cutBalance returns the balance of the cut at a sequence's position plus
// offset, rejecting seqs outside current membership.
func (b *ToolPairingBalance) cutBalance(seq int64, offset int) (bool, error) {
	index, ok := b.indexBySeq[seq]
	balanced := false
	if ok && index+offset < len(b.cutBalanced) {
		balanced = b.cutBalanced[index+offset]
	} else {
		return false, fmt.Errorf("tool-pairing balance: surface seq %d not found", seq)
	}
	return balanced, nil
}

// ToolPairingBalancedBefore reports whether the cut immediately before a
// current surface sequence is tool-pairing balanced: true when no unanswered
// tool call crosses the cut. It fails when the seq is absent from the
// current surface, a surface sequence has no matching log event, or a tool
// result has no preceding open call.
func (b *ToolPairingBalance) ToolPairingBalancedBefore(sess *session.Session, seq int64) (bool, error) {
	if err := b.synchronize(sess); err != nil {
		return false, err
	}
	return b.cutBalance(seq, 0)
}

// ToolPairingBalancedAfter reports whether the cut immediately after a
// current surface sequence is tool-pairing balanced.
func (b *ToolPairingBalance) ToolPairingBalancedAfter(sess *session.Session, seq int64) (bool, error) {
	if err := b.synchronize(sess); err != nil {
		return false, err
	}
	return b.cutBalance(seq, 1)
}

// assistantToolCallCount decodes one assistant message event's tool-call
// count (used by tests through the surface event stream).
func assistantToolCallCount(data json.RawMessage) int {
	var decoded struct {
		Message struct {
			Content []llm.ContentBlock `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return 0
	}
	count := 0
	for _, block := range decoded.Message.Content {
		if block.Type == llm.BlockToolCall {
			count++
		}
	}
	return count
}

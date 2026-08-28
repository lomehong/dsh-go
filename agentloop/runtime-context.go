package agentloop

import (
	"encoding/json"
	"fmt"

	"dshgo/llm"
	"dshgo/session"
)

// Durable projection state for dynamic runtime context.
//
// Port of packages/core/agent-loop/src/runtime-context.ts. Go adaptation: the
// source subscribes to `session/event` on the agent context; the projection
// instead folds the live log forward from a cursor each time it projects (the
// loop is the only writer of owned snapshots, and the fold is idempotent), so
// no session subscription surface is needed.

const runtimeContextSource = "@deepseek-ai/dsh-system-prompt"

const runtimeContextCleared = "Current runtime context: none. Earlier runtime-context snapshots no longer apply."

// runtimeContextRetained is the projection state: ok=false means no snapshot
// ever existed; seq<0 means none is retained.
type runtimeContextRetained struct {
	ok   bool
	seq  int64
	text string
}

// RuntimeContextProjection tracks the last retained runtime-context snapshot
// without owning its commit.
type RuntimeContextProjection struct {
	session  *session.Session
	retained runtimeContextRetained
	observed int
}

// NewRuntimeContextProjection restores projection state once, then follows
// authoritative session events.
func NewRuntimeContextProjection(sess *session.Session) *RuntimeContextProjection {
	projection := &RuntimeContextProjection{session: sess}
	events := sess.Events()
	nodes := map[int64]bool{}
	for _, seq := range sess.Surface().Nodes() {
		nodes[seq] = true
	}
	// Backward scan: the newest owned surface snapshot is the retained one;
	// any owned event at all distinguishes "none retained" from "never was".
	for index := len(events) - 1; index >= 0; index -= 1 {
		event := events[index]
		if event.Type != session.EventUserMessage {
			continue
		}
		owned, text, err := ownedRuntimeContext(event)
		if err != nil || !owned {
			continue
		}
		projection.retained.ok = true
		if nodes[event.Seq] {
			projection.retained = runtimeContextRetained{ok: true, seq: event.Seq, text: text}
			break
		}
	}
	projection.observed = len(events)
	return projection
}

// ownedRuntimeContext reports whether the event carries a runtime-context
// snapshot owned by this projection, and its single text block when so.
func ownedRuntimeContext(event session.Event) (bool, string, error) {
	message, err := session.DecodeUserMessage(event)
	if err != nil {
		return false, "", err
	}
	if message.Source.Kind != llm.SourcePlugin || message.Source.Plugin != runtimeContextSource {
		return false, "", nil
	}
	if len(message.Content) != 1 || message.Content[0].Type != llm.BlockText {
		return false, "", nil
	}
	return true, message.Content[0].Text, nil
}

// follow advances the incremental fold over newly appended events.
func (p *RuntimeContextProjection) follow() {
	events := p.session.Events()
	for index := p.observed; index < len(events); index++ {
		event := events[index]
		if event.Type == session.EventUserMessage {
			if owned, text, err := ownedRuntimeContext(event); err == nil && owned {
				p.retained = runtimeContextRetained{ok: true, seq: event.Seq, text: text}
			}
			continue
		}
		if p.retained.ok && p.retained.seq >= 0 && isReplacementSurfaceEvent(event) {
			for _, seq := range event.SourceEventSeqs {
				if seq == p.retained.seq {
					p.retained.seq = -1
					break
				}
			}
		}
	}
	p.observed = len(events)
}

// isReplacementSurfaceEvent reports whether the event shadowed an existing
// surface range instead of appending to the tail.
func isReplacementSurfaceEvent(event session.Event) bool {
	return event.SurfaceOp != nil && event.SurfaceOp.Kind != session.SurfaceAppend
}

// Project creates an uncommitted snapshot only when the retained value
// differs. current is the fully rendered dynamic context; sections are the
// named contributions that formed it. Returns a candidate user message, or
// nil when no update is needed.
func (p *RuntimeContextProjection) Project(current string, sections []llm.ContextSnapshotSection) llm.Message {
	p.follow()
	if !p.retained.ok && len(current) == 0 {
		return llm.Message{}
	}
	snapshot := current
	if len(current) == 0 {
		snapshot = runtimeContextCleared
	}
	if p.retained.ok && p.retained.text == snapshot {
		return llm.Message{}
	}
	source := llm.MessageSource{Kind: llm.SourcePlugin, Plugin: runtimeContextSource}
	if len(sections) > 0 {
		// The cleared marker has no contributions left to attribute.
		source.Form = llm.FormSnapshot
		source.Sections = sections
	}
	return llm.NewUserMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: snapshot}}, source)
}

// projectJSON is a test helper: the round-trip shape proves the projected
// message survives the session log's canonical JSON.
func projectJSON(message llm.Message) (json.RawMessage, error) {
	encoded, err := json.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("projected message: %w", err)
	}
	return encoded, nil
}

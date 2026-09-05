// Package sessionturnoutline re-implements
// @deepseek-ai/dsh-session-turn-outline (official tag dsh-v0.1.3-alpha.1):
// the whole-log turn outline projection unit — turn number, turn/start seq,
// and bounded first-prompt/final-response previews — so a client can offer
// every turn of a session and target history paging at exact seqs without
// holding the events.
package sessionturnoutline

import (
	"encoding/json"
	"regexp"
	"strings"

	"dshgo/llm"
	"dshgo/session"
	"dshgo/session/projection"
)

// ProjectionKey is the unit's registry key.
const ProjectionKey = "turnOutline"

// StateVersion guards persisted rows.
const StateVersion = 2

// Preview budgets sized to the rail card's clamps — one prompt line, up to
// three response lines — so a turn shows the same words before and after its
// events load.
const (
	PromptPreviewLimit   = 50
	ResponsePreviewLimit = 120
)

// TurnOutlineEntry is one started turn's outline facts, independent of what
// a client has paged in.
type TurnOutlineEntry struct {
	// Turn is the host-assigned turn number (the turn/start payload).
	Turn int64 `json:"turn"`
	// Seq is the turn's turn/start event seq — paging a window back through
	// this seq loads the whole turn.
	Seq int64 `json:"seq"`
	// Prompt is the bounded first-human-prompt preview (one rail-card
	// line); empty until an eligible prompt lands.
	Prompt string `json:"prompt"`
	// Response is the bounded final-response preview (up to three
	// rail-card lines); empty until the turn ends with assistant text.
	Response string `json:"response"`
}

// State is the fold state: the served entries plus the open turn's response
// draft. The draft buffers the newest text-bearing assistant message until
// turn/end commits it; draft-only applies keep the turns array's identity so
// the change feed stays quiet between turn boundaries.
type State struct {
	Turns []TurnOutlineEntry `json:"turns"`
	Draft string             `json:"draft"`
}

var whitespaceRun = regexp.MustCompile(`\s+`)

// preview space-joins text blocks, collapses whitespace, and caps at limit
// with a trailing ellipsis when clipped. Per-block bounding keeps one
// multi-megabyte block from being concatenated whole for a preview this
// short.
func preview(content []llm.ContentBlock, limit int) string {
	var text strings.Builder
	unread := false
	for _, block := range content {
		if block.Type != llm.BlockText {
			continue
		}
		if text.Len() >= limit*2 {
			unread = true
			break
		}
		clipped := len(block.Text) > limit*2
		chunk := block.Text
		if clipped {
			chunk = block.Text[:limit*2]
		}
		if text.Len() == 0 {
			text.WriteString(chunk)
		} else {
			text.WriteString(" ")
			text.WriteString(chunk)
		}
		if clipped {
			unread = true
			break
		}
	}
	normalized := strings.TrimSpace(whitespaceRun.ReplaceAllString(text.String(), " "))
	runes := []rune(normalized)
	if len(runes) > limit-1 {
		return strings.TrimRight(string(runes[:limit-1]), " ") + "…"
	}
	if unread {
		return normalized + "…"
	}
	return normalized
}

// Projection is the unit registered on a projection registry.
var Projection = projection.Unit[State]{
	Key:          ProjectionKey,
	StateVersion: StateVersion,
	Init: func(session.SessionHeader) State {
		return State{}
	},
	Apply: func(current State, event session.Event) (State, bool) {
		switch event.Type {
		case session.EventTurnStart:
			start := decode[session.TurnStartData](event)
			if last := lastEntry(current); last != nil && start.Turn <= last.Turn {
				// Order guard: a boundary that does not advance the turn
				// number keeps the outline sorted, and a retried turn's
				// previews land on the standing entry.
				return current, false
			}
			next := State{
				Turns: append(append([]TurnOutlineEntry{}, current.Turns...),
					TurnOutlineEntry{Turn: start.Turn, Seq: event.Seq}),
				Draft: "",
			}
			return next, true
		case session.EventUserMessage:
			message := decode[llm.Message](event)
			// Only the newest turn can still be waiting for its opening
			// human prompt; later human messages in the same turn (steering)
			// keep the first preview.
			if message.Source.Kind != llm.SourceUser {
				return current, false
			}
			index := len(current.Turns) - 1
			if index < 0 || current.Turns[index].Prompt != "" {
				return current, false
			}
			prompt := preview(message.Content, PromptPreviewLimit)
			if prompt == "" {
				return current, false
			}
			next := State{Turns: append(append([]TurnOutlineEntry{}, current.Turns[:index]...), current.Turns[index])}
			next.Turns[index].Prompt = prompt
			next.Draft = current.Draft
			return next, true
		case session.EventAssistantMsg:
			message := decode[session.AssistantMessageData](event)
			// Newest text-bearing message wins; the buffer commits at
			// turn/end.
			draft := preview(message.Message.Content, ResponsePreviewLimit)
			if draft == "" || draft == current.Draft {
				return current, false
			}
			return State{Turns: current.Turns, Draft: draft}, true
		case session.EventTurnEnd:
			if current.Draft == "" {
				return current, false
			}
			index := len(current.Turns) - 1
			if index < 0 || current.Turns[index].Response == current.Draft {
				return State{Turns: current.Turns, Draft: ""}, false
			}
			next := State{Turns: append(append([]TurnOutlineEntry{}, current.Turns[:index]...), current.Turns[index]), Draft: ""}
			next.Turns[index].Response = current.Draft
			return next, true
		default:
			return current, false
		}
	},
	// The wire view is the entries array itself (official view:
	// state => state.turns); draft-only applies keep array identity so the
	// identity-gated change feed stays quiet between turn boundaries.
	View: func(folded State) any {
		return folded.Turns
	},
	DecodeState: decodeState,
}

// decodeState validates and reifies a persisted state row.
func decodeState(raw json.RawMessage) (State, error) {
	var state State
	if err := json.Unmarshal(raw, &state); err != nil {
		return State{}, err
	}
	previous := int64(-1)
	for _, entry := range state.Turns {
		if entry.Turn <= previous {
			return State{}, errOutlineOrder
		}
		previous = entry.Turn
	}
	return state, nil
}

var errOutlineOrder = jsonInvalid("turn outline entries must be strictly increasing by turn")

func jsonInvalid(message string) error { return &outlineError{message} }

type outlineError struct{ message string }

func (e *outlineError) Error() string { return e.message }

func lastEntry(state State) *TurnOutlineEntry {
	if len(state.Turns) == 0 {
		return nil
	}
	return &state.Turns[len(state.Turns)-1]
}

func decode[T any](event session.Event) T {
	var decoded T
	if err := json.Unmarshal(event.Data, &decoded); err != nil {
		decoded = *new(T)
	}
	return decoded
}

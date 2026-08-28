package subagent

import (
	"encoding/json"

	"dshgo/llm"
	"dshgo/session"
)

// AssistantOutputFold is the incremental fold of the canonical selection
// rule, for backends that observe a child's output as it streams:
// session-event backends Push each event, and transports without session
// events (ACP content chunks) PushText raw text into the same streamed
// fallback.
type AssistantOutputFold struct {
	message []llm.ContentBlock
	partial []string
}

// Push folds one session event: a non-empty assistant message becomes the
// candidate final answer, and a `text-delta` chunk extends the streamed
// fallback; every other event contributes nothing.
func (f *AssistantOutputFold) Push(event session.Event) {
	if event.Type == session.EventAssistantMsg {
		data, err := session.DecodeAssistantMessage(event)
		if err != nil {
			return
		}
		if len(data.Message.Content) > 0 {
			f.message = data.Message.Content
		}
		return
	}
	if event.Type == session.EventAssistantChunk {
		// The chunk payload's Go value type is loop-private; decode by its
		// stable wire keys.
		var data struct {
			Chunk llm.StreamChunk `json:"chunk"`
		}
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return
		}
		if data.Chunk.Type == llm.ChunkTextDelta {
			f.PushText(data.Chunk.Text)
		}
	}
}

// PushText extends the streamed fallback with text observed outside session
// events. An empty piece is a no-op.
func (f *AssistantOutputFold) PushText(text string) {
	if len(text) > 0 {
		f.partial = append(f.partial, text)
	}
}

// Collect selects the final output folded so far: the last non-empty
// assistant message, else the accumulated streamed text, or nil when the
// child produced neither.
func (f *AssistantOutputFold) Collect() []llm.ContentBlock {
	if f.message != nil {
		return f.message
	}
	text := ""
	for _, piece := range f.partial {
		text += piece
	}
	if len(text) > 0 {
		return []llm.ContentBlock{{Type: llm.BlockText, Text: text}}
	}
	return nil
}

// FinalAssistantOutput applies the selection rule to one complete
// child-owned event suffix (after any seed or epoch boundary). Selection is
// independent of the run's stop reason. An empty-content message records
// usage only when the loop appends it after a max-tokens step with no
// executable blocks, so it does not replace earlier output.
func FinalAssistantOutput(events []session.Event) []llm.ContentBlock {
	var fold AssistantOutputFold
	for _, event := range events {
		fold.Push(event)
	}
	return fold.Collect()
}

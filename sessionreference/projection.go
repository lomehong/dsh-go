package sessionreference

import (
	"fmt"
	"strings"

	"dshgo/compaction"
	"dshgo/llm"
	"dshgo/outputretention"
)

// EventKind discriminates the surface events the projection reads.
type EventKind string

// SurfaceEventKind values: user/message, assistant/message, tool/result.
const (
	EventUserMessage      EventKind = "user/message"
	EventAssistantMessage EventKind = "assistant/message"
	EventToolResult       EventKind = "tool/result"
)

// SurfaceUserMessage is one user/message event's payload.
type SurfaceUserMessage struct {
	Source  llm.MessageSource
	Content []llm.ContentBlock
}

// SurfaceAssistantMessage is one assistant/message event's payload.
type SurfaceAssistantMessage struct {
	Content []llm.ContentBlock
}

// SurfaceEvent is one current-surface event the projection reads; other
// event kinds never reach this seam.
type SurfaceEvent struct {
	Type      EventKind
	User      *SurfaceUserMessage
	Assistant *SurfaceAssistantMessage
}

// SessionSnapshot is the current-surface source observation: session facts
// plus the projected conversation events.
type SessionSnapshot struct {
	SessionID string
	// Cwd renders as null when empty.
	Cwd string
	// CapturedThroughSeq renders as null when Has is false.
	CapturedThroughSeq    int64
	HasCapturedThroughSeq bool
	Events                []SurfaceEvent
}

// ReferencedConversationItem is one text-only projected conversation item.
type ReferencedConversationItem struct {
	// Role: "user" | "assistant".
	Role string `json:"role"`
	// Text is the visible text retained from that message.
	Text string `json:"text"`
}

// ReferencedSessionData is the snapshot data serialized inside the
// untrusted prompt.
type ReferencedSessionData struct {
	SessionID          string                       `json:"sessionId"`
	Label              string                       `json:"label"`
	Cwd                *string                      `json:"cwd"`
	CapturedThroughSeq *int64                       `json:"capturedThroughSeq"`
	Conversation       []ReferencedConversationItem `json:"conversation"`
}

// ReferenceRetentionStats are the retention facts stored beside the durable
// context.
type ReferenceRetentionStats struct {
	Compacted        bool `json:"compacted"`
	OriginalMessages int  `json:"originalMessages"`
	RetainedMessages int  `json:"retainedMessages"`
	OmittedMessages  int  `json:"omittedMessages"`
	OmittedBytes     int  `json:"omittedBytes"`
	Truncated        bool `json:"truncated"`
}

// RetainedReference couples the serialized data with its stats.
type RetainedReference struct {
	Data  ReferencedSessionData
	Stats ReferenceRetentionStats
}

// projectedItem carries retention bookkeeping beside the visible item.
type projectedItem struct {
	role        string
	text        string
	checkpoint  bool
	omittedText int
}

// projectSessionConversation projects current user/assistant conversation
// while excluding tools, reasoning, and injected context. Compaction
// checkpoints ride along flagged so retention can pin them.
func projectSessionConversation(snapshot SessionSnapshot) []projectedItem {
	conversation := []projectedItem{}
	for _, event := range snapshot.Events {
		switch event.Type {
		case EventUserMessage:
			checkpoint := compaction.IsCompactCheckpointSource(event.User.Source)
			if !checkpoint && event.User.Source.Kind != "user" {
				continue
			}
			text := textContent(event.User.Content)
			if text != "" {
				conversation = append(conversation, projectedItem{role: "user", text: text, checkpoint: checkpoint})
			}
		case EventAssistantMessage:
			text := textContent(event.Assistant.Content)
			if text != "" {
				conversation = append(conversation, projectedItem{role: "assistant", text: text})
			}
		case EventToolResult:
		}
	}
	return conversation
}

// textContent joins the text blocks of one content list with newlines.
func textContent(blocks []llm.ContentBlock) string {
	texts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Type == llm.BlockText {
			texts = append(texts, block.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// RetainReferencedSession fits one projected snapshot into an exact rendered
// JSON-object byte cap: whole non-checkpoint messages (oldest first, newest
// pinned) drop first, then the longest retained message shortens under a
// head-tail notice. The result is nil when the fixed data cannot fit.
func RetainReferencedSession(snapshot SessionSnapshot, label string, maxBytes int) *RetainedReference {
	original := projectSessionConversation(snapshot)
	retained := make([]projectedItem, len(original))
	copy(retained, original)
	omittedMessages := 0
	droppedOmittedBytes := 0

	data := func() ReferencedSessionData {
		conversation := make([]ReferencedConversationItem, 0, len(retained))
		for _, item := range retained {
			conversation = append(conversation, ReferencedConversationItem{Role: item.role, Text: item.text})
		}
		envelope := ReferencedSessionData{
			SessionID:    snapshot.SessionID,
			Label:        label,
			Conversation: conversation,
		}
		if snapshot.Cwd != "" {
			cwd := snapshot.Cwd
			envelope.Cwd = &cwd
		}
		if snapshot.HasCapturedThroughSeq {
			seq := snapshot.CapturedThroughSeq
			envelope.CapturedThroughSeq = &seq
		}
		return envelope
	}
	size := func() int {
		serialized, err := StringifyTagSafeJSON(data())
		if err != nil {
			return maxBytes + 1
		}
		return len([]byte(serialized))
	}

	// Whole-message drops: oldest first, never the newest, checkpoints
	// pinned.
	for size() > maxBytes {
		dropIndex := -1
		for index, item := range retained {
			if !item.checkpoint && index != len(retained)-1 {
				dropIndex = index
				break
			}
		}
		if dropIndex < 0 {
			break
		}
		removed := retained[dropIndex]
		retained = append(retained[:dropIndex], retained[dropIndex+1:]...)
		omittedMessages++
		droppedOmittedBytes += len([]byte(removed.text))
	}

	// Byte shortening: the longest message shrinks under a head-tail
	// notice until the envelope fits or nothing can give.
	for size() > maxBytes {
		longestIndex := -1
		longestBytes := 0
		for index, item := range retained {
			if bytes := len([]byte(item.text)); bytes > longestBytes {
				longestBytes = bytes
				longestIndex = index
			}
		}
		if longestIndex < 0 || longestBytes == 0 {
			return nil
		}
		overflow := size() - maxBytes
		target := longestBytes - overflow
		if target < 0 {
			target = 0
		}
		shortened, omitted := truncateWithNotice(retained[longestIndex].text, target)
		if shortened == retained[longestIndex].text {
			return nil
		}
		retained[longestIndex].text = shortened
		retained[longestIndex].omittedText = omitted
	}

	compacted := false
	retainedOmittedBytes := 0
	for _, item := range original {
		if item.checkpoint {
			compacted = true
			break
		}
	}
	for _, item := range retained {
		retainedOmittedBytes += item.omittedText
	}
	return &RetainedReference{
		Data: data(),
		Stats: ReferenceRetentionStats{
			Compacted:        compacted,
			OriginalMessages: len(original),
			RetainedMessages: len(retained),
			OmittedMessages:  omittedMessages,
			OmittedBytes:     retainedOmittedBytes + droppedOmittedBytes,
			Truncated:        omittedMessages > 0 || retainedOmittedBytes+droppedOmittedBytes > 0,
		},
	}
}

// truncateWithNotice shortens one text to a rendered byte target: a
// binary-searched head-tail cut plus the omitted-bytes notice must fit the
// target. The result is the best candidate found.
func truncateWithNotice(text string, maxOutputBytes int) (string, int) {
	if len([]byte(text)) <= maxOutputBytes {
		return text, 0
	}
	low, high := 0, maxOutputBytes
	best := ""
	bestOmitted := len([]byte(text))
	for low <= high {
		retainedBytes := (low + high) / 2
		headBytes := (retainedBytes + 1) / 2
		tailBytes := retainedBytes / 2
		retainer := outputretention.NewTextRetainer(outputretention.TextStrategy{
			Kind: "headTail", HeadBytes: headBytes, TailBytes: tailBytes,
		})
		retainer.PushString(text)
		result := retainer.Finish()
		// The complete source string was pushed before Finish, so omission
		// is exact.
		if result.OmittedBytes.Kind != outputretention.OmittedExact {
			panic("session-reference retention did not report exact omitted bytes")
		}
		candidate := fmt.Sprintf("%s\n[… omitted %d UTF-8 bytes …]", result.Text, result.OmittedBytes.Count)
		if len([]byte(candidate)) <= maxOutputBytes {
			best = candidate
			bestOmitted = result.OmittedBytes.Count
			low = retainedBytes + 1
		} else {
			high = retainedBytes - 1
		}
	}
	return best, bestOmitted
}

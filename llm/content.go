package llm

import (
	"encoding/json"
	"fmt"
)

// Durable file history projection. Port of packages/llm/llm/src/content.ts
// (0.1.3-alpha.1): files never reach a provider natively — request assembly
// projects every occurrence to deterministic handle text (name, byte size,
// and the read-only saved path), so adapters and providers see text in its
// place while the durable log keeps the structured reference for
// presentation and authorization.

// BlockFile marks a durable verbatim file reference block, valid in user
// content (official FileBlock).
const BlockFile = "file"

// FileRef extracts the durable file reference from one file block.
func FileRef(block ContentBlock) (attachmentID string, name string, bytes int, ok bool) {
	if block.Type != BlockFile {
		return "", "", 0, false
	}
	ref, ok := block.Attachment.(map[string]any)
	if !ok {
		return "", "", 0, false
	}
	id, _ := ref["attachmentId"].(string)
	display, _ := ref["name"].(string)
	if id == "" {
		return "", "", 0, false
	}
	size := 0
	if number, isNumber := ref["bytes"]; isNumber {
		if parsed, err := numberToInt(number); err == nil {
			size = parsed
		}
	}
	return id, display, size, true
}

// FileRefFromRaw decodes one file block's reference from raw JSON.
func FileRefFromRaw(raw json.RawMessage) (attachmentID, name string, bytes int, ok bool) {
	var block ContentBlock
	if err := json.Unmarshal(raw, &block); err != nil {
		return "", "", 0, false
	}
	return FileRef(block)
}

func numberToInt(value any) (int, error) {
	switch typed := value.(type) {
	case int:
		return typed, nil
	case int64:
		return int(typed), nil
	case float64:
		return int(typed), nil
	default:
		return 0, fmt.Errorf("not a number")
	}
}

// ContentHasFile reports whether typed model content contains a file block,
// walking nested tool-result content on the same recursion every file policy
// shares.
func ContentHasFile(content []ContentBlock) bool {
	for _, block := range content {
		if block.Type == BlockFile {
			return true
		}
		if block.Type == BlockToolResult && ContentHasFile(block.Content) {
			return true
		}
	}
	return false
}

// quotedJSON renders one string exactly as JSON.stringify would.
func quotedJSON(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return `"` + value + `"`
	}
	return string(encoded)
}

// FileHandleText builds the stable model-facing handle for one durable file
// reference: the address of the verbatim stored copy and the instruction to
// read it on demand. This is the only representation a provider ever
// receives for a file (official fileHandleText).
func FileHandleText(attachmentID, name string, bytes int, readonlyPath string) string {
	digest := attachmentID
	prefix := len("sha256:")
	if len(digest) >= prefix+8 {
		digest = digest[prefix : prefix+8]
	}
	identity := fmt.Sprintf("File %s (%d bytes, sha256:%s)", quotedJSON(name), bytes, digest)
	if readonlyPath == "" {
		return fmt.Sprintf("[%s was uploaded, but the current execution environment cannot access a readable path. Report that limitation if its contents are needed; do not claim to have read it.]", identity)
	}
	return fmt.Sprintf("[%s: verbatim read-only copy saved at %s. Read that path with your file tools when its contents are needed; copy it to a writable location before modifying it. When delegating file work, include this saved path in the delegation prompt; only subagents sharing this execution environment can read it.]", identity, quotedJSON(readonlyPath))
}

// replaceFilesWithHandles replaces every file occurrence, including nested
// tool results, with handle text; returns the input slice untouched when no
// replacement occurred.
func replaceFilesWithHandles(blocks []ContentBlock, resolvePath func(attachmentID string) string) []ContentBlock {
	var next []ContentBlock
	for index, block := range blocks {
		if block.Type == BlockFile {
			if next == nil {
				next = append(next, blocks[:index]...)
			}
			id, name, size, ok := FileRef(block)
			var path string
			if ok && resolvePath != nil {
				path = resolvePath(id)
			}
			next = append(next, ContentBlock{Type: BlockText, Text: FileHandleText(id, name, size, path)})
			continue
		}
		if block.Type == BlockToolResult {
			content := replaceFilesWithHandles(block.Content, resolvePath)
			if len(content) != len(block.Content) || !sameBlocks(content, block.Content) {
				if next == nil {
					next = append(next, blocks[:index]...)
				}
				block.Content = content
				next = append(next, block)
				continue
			}
		}
		if next != nil {
			next = append(next, block)
		}
	}
	if next == nil {
		return blocks
	}
	return next
}

func sameBlocks(left, right []ContentBlock) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Type != right[index].Type || left[index].Text != right[index].Text ||
			left[index].ID != right[index].ID || left[index].Name != right[index].Name {
			return false
		}
	}
	return true
}

// ProjectFilesToText projects durable file history into deterministic handle
// text for every model route. Unlike images, no provider receives file
// blocks natively, so this projection is unconditional in request assembly.
// resolvePath resolves one reference's current execution-world read path.
func ProjectFilesToText(messages []Message, resolvePath func(attachmentID string) string) []Message {
	any := false
	for _, message := range messages {
		if ContentHasFile(message.Content) {
			any = true
			break
		}
	}
	if !any {
		return messages
	}
	projected := make([]Message, 0, len(messages))
	for _, message := range messages {
		content := replaceFilesWithHandles(message.Content, resolvePath)
		message.Content = content
		projected = append(projected, message)
	}
	return projected
}

// ContentHasFileInMessages reports whether any message's content carries a
// file block.
func ContentHasFileInMessages(messages []Message) bool {
	for _, message := range messages {
		if ContentHasFile(message.Content) {
			return true
		}
	}
	return false
}

package sessionformatv12

import (
	"encoding/json"

	"dshgo/llm"
	"dshgo/sessionformat"
)

// Shared v12 helpers.

func itoa(value int64) string { return itoaLiteral(value) }

func itoaLiteral(value int64) string {
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

// numberOf renders an int64 as a lossless JSON number tree value.
func numberOf(value int64) any {
	tree, err := sessionformat.DecodeValue(json.RawMessage(itoaLiteral(value)))
	if err != nil {
		panic(err)
	}
	return tree
}

// mustDecode decodes one raw JSON value into a tree; used on values the
// encoder itself produced, so failures are panics.
func mustDecode(raw []byte) any {
	tree, err := sessionformat.DecodeValue(raw)
	if err != nil {
		panic(err)
	}
	return tree
}

func mustJSON(value any) json.RawMessage {
	encoded, err := sessionformat.EncodeValue(value)
	if err != nil {
		panic(err)
	}
	return encoded
}

// deepEqualJSON compares two JSON trees canonically (official
// deepEqualJson). Go's encoder sorts map keys, so equal trees marshal
// identically; numeric literals compare textually (1 vs 1.0 differ — a
// recorded adaptation, since logs carry canonical literals).
func deepEqualJSON(left, right any) bool {
	leftPresent := left != nil
	rightPresent := right != nil
	if !leftPresent || !rightPresent {
		return leftPresent == rightPresent
	}
	leftEncoded, err := sessionformat.EncodeValue(left)
	if err != nil {
		return false
	}
	rightEncoded, err := sessionformat.EncodeValue(right)
	if err != nil {
		return false
	}
	return string(leftEncoded) == string(rightEncoded)
}

// blocksToJSON renders assembled content blocks as a comparable JSON tree.
func blocksToJSON(blocks []llm.ContentBlock) any {
	if blocks == nil {
		blocks = []llm.ContentBlock{}
	}
	encoded, err := json.Marshal(blocks)
	if err != nil {
		return nil
	}
	return mustDecode(encoded)
}

// usageToJSON renders usage as a comparable JSON tree (nil = absent).
func usageToJSON(usage *llm.TokenUsage) any {
	if usage == nil {
		return nil
	}
	encoded, err := json.Marshal(usage)
	if err != nil {
		return nil
	}
	return mustDecode(encoded)
}

// replayToJSON renders the replay envelope as a comparable JSON tree.
func replayToJSON(replay *llm.ReplayEnvelope) any {
	if replay == nil {
		return nil
	}
	encoded, err := json.Marshal(replay)
	if err != nil {
		return nil
	}
	return mustDecode(encoded)
}

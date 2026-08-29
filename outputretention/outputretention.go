// Package outputretention ports @deepseek-ai/dsh-output-retention: bounded
// model-facing output for tools that must cap how much context they return.
// A caller feeds items or text chunks into a bounded object, then gets the
// retained content plus exact omission metadata.
//
// The library owns only the mechanical question "what did we keep, what did
// we omit?". Tool-specific code still owns business semantics: file
// grouping, line numbering, exit codes, provider error states, spill files,
// and the model-facing prose. Truncated means "the retainer omitted
// otherwise-available content because of a budget" — never "the upstream was
// incomplete".
package outputretention

import (
	"fmt"
	"strings"
)

// OmittedKind discriminates how much content the retainer omitted.
type OmittedKind string

// OmittedNone: nothing was dropped. OmittedExact: every unit/byte was
// observed, so the count is precise. OmittedUnknown is reserved for a caller
// that omits without a count; the retainers never return it.
const (
	OmittedNone    OmittedKind = "none"
	OmittedExact   OmittedKind = "exact"
	OmittedUnknown OmittedKind = "unknown"
)

// Omitted is the omission metadata attached to every retainer result.
type Omitted struct {
	Kind  OmittedKind
	Count int // valid only for OmittedExact
}

// NoneOmitted is the shared nothing-dropped value.
func NoneOmitted() Omitted { return Omitted{Kind: OmittedNone} }

// ExactOmitted builds the precise-omission value.
func ExactOmitted(count int) Omitted { return Omitted{Kind: OmittedExact, Count: count} }

// PushDecision is the caller's per-push report.
type PushDecision struct {
	// Kept reports whether this whole unit / all of this chunk's bytes
	// were retained.
	Kept bool
	// Truncated reports, cumulatively, whether the retainer has omitted
	// anything due to the budget yet.
	Truncated bool
}

// RetainedItems is the final result for ordered logical units. Seen counts
// units OBSERVED by the retainer, not necessarily the total upstream; Kept
// equals len(Items), surfaced so a notice formatter need not re-count.
type RetainedItems[T any] struct {
	Items     []T
	Truncated bool
	Seen      int
	Kept      int
	Omitted   Omitted
}

// RetainedText is the final result for text streams. The text carries no
// tool-specific headers, exit markers, or recovery instructions, and
// OmittedBytes counts BYTES — text retention is byte-oriented. UTF-8
// boundaries at each cut are preserved, so the text never carries a
// replacement char introduced by the cut itself.
type RetainedText struct {
	Text         string
	Truncated    bool
	OmittedBytes Omitted
}

// ItemStrategy is the item retention strategy: keep the first MaxItems
// units (head only in v1; windows/grouped budgets wait for a second
// consumer).
type ItemStrategy struct {
	MaxItems int
}

func assertBudget(value int, name string) {
	if value < 0 {
		panic(fmt.Sprintf("%s must be a non-negative integer", name))
	}
}

// ItemRetainer bounds an ordered stream of logical units, keeping the first
// MaxItems. Grouping, sorting, path mapping, per-unit preview truncation,
// and any incomplete state stay outside the retainer: it counts and keeps,
// nothing more.
type ItemRetainer[T any] struct {
	maxItems     int
	items        []T
	seen         int
	omittedCount int
}

// NewItemRetainer builds a head retainer. MaxItems must be a non-negative
// integer (the retainer request contract).
func NewItemRetainer[T any](strategy ItemStrategy) *ItemRetainer[T] {
	assertBudget(strategy.MaxItems, "maxItems")
	return &ItemRetainer[T]{maxItems: strategy.MaxItems}
}

// Push offers one unit. Kept while below MaxItems; otherwise dropped and
// counted. Callers keep pushing all observed units, so the final omission
// count is exact.
func (r *ItemRetainer[T]) Push(item T) PushDecision {
	r.seen++
	if len(r.items) < r.maxItems {
		r.items = append(r.items, item)
		return PushDecision{Kept: true, Truncated: false}
	}
	r.omittedCount++
	return PushDecision{Kept: false, Truncated: true}
}

// Finish reports what was kept and omitted.
func (r *ItemRetainer[T]) Finish() RetainedItems[T] {
	truncated := r.omittedCount > 0
	omitted := NoneOmitted()
	if truncated {
		omitted = ExactOmitted(r.omittedCount)
	}
	return RetainedItems[T]{
		Items:     r.items,
		Truncated: truncated,
		Seen:      r.seen,
		Kept:      len(r.items),
		Omitted:   omitted,
	}
}

// TextStrategy is the text retention strategy: keep a prefix, a suffix, or
// both, counted in bytes.
type TextStrategy struct {
	// Kind: "head", "tail", or "headTail".
	Kind string
	// MaxBytes bounds head and tail strategies.
	MaxBytes int
	// HeadBytes bounds the prefix of a headTail strategy.
	HeadBytes int
	// TailBytes bounds the suffix of a headTail strategy.
	TailBytes int
}

// TextRetainer bounds a byte-oriented text stream, keeping a prefix, a
// suffix, or both. Bytes, not characters: caps and omission counts are byte
// counts. Chunks that straddle a codepoint are handled — Finish trims a
// partial codepoint at each cut. The retainer holds at most prefix+suffix
// caps plus one chunk (old suffix chunks drop as they slide out), so a large
// stream does not accumulate unbounded.
type TextRetainer struct {
	prefixCap    int
	suffixCap    int
	prefixChunks [][]byte
	prefixHeld   int
	suffixChunks [][]byte
	suffixHeld   int
	total        int
}

// NewTextRetainer builds one retainer. Byte budgets must be non-negative
// integers.
func NewTextRetainer(strategy TextStrategy) *TextRetainer {
	switch strategy.Kind {
	case "head":
		assertBudget(strategy.MaxBytes, "maxBytes")
		return &TextRetainer{prefixCap: strategy.MaxBytes}
	case "tail":
		assertBudget(strategy.MaxBytes, "maxBytes")
		return &TextRetainer{suffixCap: strategy.MaxBytes}
	case "headTail":
		assertBudget(strategy.HeadBytes, "headBytes")
		assertBudget(strategy.TailBytes, "tailBytes")
		return &TextRetainer{prefixCap: strategy.HeadBytes, suffixCap: strategy.TailBytes}
	default:
		panic(fmt.Sprintf("unknown text retention strategy %q", strategy.Kind))
	}
}

// Push offers one chunk: UTF-8 bytes or a Go string. Prefix bytes fill up to
// the prefix cap then stop; suffix bytes roll so only the last suffixCap
// bytes are retained. Kept is true only when no byte of this chunk was
// dropped.
func (r *TextRetainer) Push(chunk []byte) PushDecision {
	before := r.total
	r.total += len(chunk)

	// Prefix: take only up to the cap; the rest of this chunk is "not
	// prefixed".
	if room := r.prefixCap - r.prefixHeld; room > 0 {
		take := room
		if take > len(chunk) {
			take = len(chunk)
		}
		if take > 0 {
			r.prefixChunks = append(r.prefixChunks, chunk[:take])
			r.prefixHeld += take
		}
	}

	// Suffix: append the whole chunk, then drop whole leading chunks that
	// have fully slid out of the last suffixCap bytes (bounded memory).
	if r.suffixCap > 0 {
		r.suffixChunks = append(r.suffixChunks, chunk)
		r.suffixHeld += len(chunk)
		head := r.suffixChunks[0]
		for len(r.suffixChunks) > 0 && r.suffixHeld-len(head) >= r.suffixCap {
			r.suffixChunks = r.suffixChunks[1:]
			r.suffixHeld -= len(head)
			head = r.suffixChunks[0]
		}
		// The head chunk can still hold leading bytes beyond the last
		// suffixCap — a single chunk larger than the window is retained
		// whole by the loop above; trim those leading bytes so the
		// accumulator stays bounded. finish() only ever reads the last
		// suffixLen bytes, so this drops nothing it would return.
		if len(r.suffixChunks) > 0 && r.suffixHeld > r.suffixCap {
			excess := r.suffixHeld - r.suffixCap
			r.suffixChunks[0] = head[excess:]
			r.suffixHeld -= excess
		}
	}

	droppedThisChunk := r.omittedAt(r.total) > r.omittedAt(before)
	return PushDecision{Kept: !droppedThisChunk, Truncated: r.omittedAt(r.total) > 0}
}

// PushString offers one string chunk (encoded UTF-8).
func (r *TextRetainer) PushString(chunk string) PushDecision {
	return r.Push([]byte(chunk))
}

// omittedAt reports bytes omitted once total bytes have been seen:
// total − keptPrefix − keptSuffix.
func (r *TextRetainer) omittedAt(total int) int {
	prefixLen := min(total, r.prefixCap)
	suffixLen := min(total-prefixLen, r.suffixCap)
	return total - prefixLen - suffixLen
}

// Finish decodes the retained prefix and suffix (each trimmed to a UTF-8
// boundary at its cut) and reports the exact omitted byte count.
func (r *TextRetainer) Finish() RetainedText {
	prefixLen := min(r.total, r.prefixCap)
	suffixLen := min(r.total-prefixLen, r.suffixCap)

	prefix := concat(r.prefixChunks) // exactly prefixLen bytes
	var suffix []byte
	if suffixLen > 0 {
		suffix = concat(r.suffixChunks)[r.suffixHeld-suffixLen:]
	}

	// With nothing omitted by budget, prefix and suffix are ADJACENT slices
	// of one stream, so the head|tail split is artificial: a codepoint may
	// span it. Decode the contiguous whole as one buffer. Only a real
	// omitted gap makes each side a true cut: trim each to a UTF-8 boundary
	// and decode separately so a codepoint is never reconstructed across
	// the gap.
	budgetOmitted := r.omittedAt(r.total)
	var text string
	if budgetOmitted > 0 {
		keptPrefix := trimTrailingPartialUtf8(prefix)
		keptSuffix := trimLeadingContinuationUtf8(suffix)
		text = string(keptPrefix) + string(keptSuffix)
		// Report omission against the bytes ACTUALLY returned, not the
		// pre-trim budget: a boundary trim drops partial-codepoint bytes
		// too, so a budget-only count would overstate the retained text.
		omitted := r.total - len(keptPrefix) - len(keptSuffix)
		if omitted > 0 {
			return RetainedText{Text: text, Truncated: true, OmittedBytes: ExactOmitted(omitted)}
		}
		return RetainedText{Text: text}
	}
	text = string(concat([][]byte{prefix, suffix}))
	return RetainedText{Text: text}
}

// trimTrailingPartialUtf8 drops a trailing incomplete UTF-8 sequence so a
// prefix cut never emits a replacement char at the boundary. It walks back
// over continuation bytes to the lead byte; if fewer bytes follow it than
// the lead byte's length declares, the sequence is incomplete and is
// trimmed. A complete tail, or a run too long/short to be a valid lead, is
// returned untouched.
func trimTrailingPartialUtf8(bytes []byte) []byte {
	i := len(bytes) - 1
	// Continuation bytes are 0b10xxxxxx; scan back at most 3 (max sequence
	// is 4).
	for i >= 0 && bytes[i]&0xc0 == 0x80 && len(bytes)-i <= 3 {
		i--
	}
	if i < 0 {
		return bytes
	}
	lead := bytes[i]
	expected := 0
	switch {
	case lead < 0x80:
		expected = 1
	case lead < 0xe0:
		expected = 2
	case lead < 0xf0:
		expected = 3
	case lead < 0xf8:
		expected = 4
	}
	// expected 0 → not a lead byte (stray continuation / invalid): leave it.
	if expected == 0 {
		return bytes
	}
	if len(bytes)-i < expected {
		return bytes[:i]
	}
	return bytes
}

// trimLeadingContinuationUtf8 drops leading continuation bytes so a suffix
// cut starts on a lead/ASCII byte instead of mid-codepoint.
func trimLeadingContinuationUtf8(bytes []byte) []byte {
	i := 0
	for i < len(bytes) && bytes[i]&0xc0 == 0x80 {
		i++
	}
	return bytes[i:]
}

func concat(chunks [][]byte) []byte {
	length := 0
	for _, chunk := range chunks {
		length += len(chunk)
	}
	out := make([]byte, 0, length)
	for _, chunk := range chunks {
		out = append(out, chunk...)
	}
	return out
}

// DescribeOmitted renders the standardized, false-precision-safe wording for
// one Omitted value: exact prints the count ("Omitted 3 items"), unknown
// prints no count because the caller did not provide one, none is empty.
func DescribeOmitted(omitted Omitted, unit string) string {
	switch omitted.Kind {
	case OmittedNone:
		return ""
	case OmittedExact:
		return fmt.Sprintf("Omitted %d %s.", omitted.Count, unit)
	case OmittedUnknown:
		return fmt.Sprintf("More %s were omitted.", unit)
	default:
		return ""
	}
}

// RetentionNotice is a neutral, tool-agnostic description of one retention
// outcome — the input to FormatRetentionNotice. It carries the mechanical
// facts; the tool supplies the recovery words, because only the tool knows
// the recovery action.
type RetentionNotice struct {
	Scope    string // tool/scope label, e.g. "grep", "bash stdout"
	Strategy string // "head" | "tail" | "headTail"
	Unit     string // "items" | "bytes" | "chars" | "lines"
	Limit    int
	Kept     int
	Omitted  Omitted
}

// FormatRetentionNotice renders a one-line footer: the standardized omission
// clause followed by the tool's own recovery guidance. Either half may be
// empty; the two are joined with a single space.
func FormatRetentionNotice(notice RetentionNotice, recovery func(RetentionNotice) string) string {
	parts := make([]string, 0, 2)
	if clause := DescribeOmitted(notice.Omitted, notice.Unit); clause != "" {
		parts = append(parts, clause)
	}
	if recovery != nil {
		if guidance := recovery(notice); guidance != "" {
			parts = append(parts, guidance)
		}
	}
	return strings.Join(parts, " ")
}

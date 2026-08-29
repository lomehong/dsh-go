package outputretention

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestItemRetainerKeepsHeadAndCountsExactly(t *testing.T) {
	retainer := NewItemRetainer[string](ItemStrategy{MaxItems: 2})
	decisions := []PushDecision{}
	for _, item := range []string{"a", "b", "c", "d"} {
		decisions = append(decisions, retainer.Push(item))
	}
	if !decisions[0].Kept || !decisions[1].Kept || decisions[2].Kept || !decisions[2].Truncated {
		t.Fatalf("decisions = %+v", decisions)
	}
	result := retainer.Finish()
	if len(result.Items) != 2 || result.Items[0] != "a" || result.Items[1] != "b" {
		t.Fatalf("items = %v", result.Items)
	}
	if !result.Truncated || result.Seen != 4 || result.Kept != 2 ||
		result.Omitted.Kind != OmittedExact || result.Omitted.Count != 2 {
		t.Fatalf("result = %+v", result)
	}
	// Under the cap nothing is truncated and omission is none.
	empty := NewItemRetainer[int](ItemStrategy{MaxItems: 3})
	empty.Push(1)
	done := empty.Finish()
	if done.Truncated || done.Omitted.Kind != OmittedNone || done.Seen != 1 || done.Kept != 1 {
		t.Fatalf("under cap = %+v", done)
	}
}

func TestTextRetainerHeadAndTail(t *testing.T) {
	// Head keeps the prefix.
	head := NewTextRetainer(TextStrategy{Kind: "head", MaxBytes: 5})
	head.PushString("hello world")
	result := head.Finish()
	if result.Text != "hello" || !result.Truncated || result.OmittedBytes.Count != 6 {
		t.Fatalf("head = %+v", result)
	}
	// Tail keeps the suffix.
	tail := NewTextRetainer(TextStrategy{Kind: "tail", MaxBytes: 5})
	tail.PushString("hello world")
	result = tail.Finish()
	if result.Text != "world" || !result.Truncated || result.OmittedBytes.Count != 6 {
		t.Fatalf("tail = %+v", result)
	}
	// Nothing omitted → no truncation, exact text.
	full := NewTextRetainer(TextStrategy{Kind: "head", MaxBytes: 100})
	full.PushString("hello world")
	result = full.Finish()
	if result.Text != "hello world" || result.Truncated {
		t.Fatalf("full = %+v", result)
	}
}

func TestTextRetainerHeadTailOmitsTheMiddle(t *testing.T) {
	retainer := NewTextRetainer(TextStrategy{Kind: "headTail", HeadBytes: 5, TailBytes: 5})
	retainer.PushString("0123456789")
	result := retainer.Finish()
	if result.Text != "0123456789" {
		t.Fatalf("within caps = %+v", result)
	}
	retainer2 := NewTextRetainer(TextStrategy{Kind: "headTail", HeadBytes: 4, TailBytes: 4})
	retainer2.PushString("0123456789")
	result = retainer2.Finish()
	if result.Text != "01236789" || !result.Truncated || result.OmittedBytes.Count != 2 {
		t.Fatalf("headTail = %+v", result)
	}
}

func TestTextRetainerUTF8Boundaries(t *testing.T) {
	// "héllo" — é is 2 bytes. A head cut mid-codepoint must not emit U+FFFD.
	retainer := NewTextRetainer(TextStrategy{Kind: "head", MaxBytes: 2})
	retainer.PushString("héllo")
	result := retainer.Finish()
	if result.Text != "h" || !utf8.ValidString(result.Text) {
		t.Fatalf("head cut = %q", result.Text)
	}
	// Tail cut mid-codepoint.
	tail := NewTextRetainer(TextStrategy{Kind: "tail", MaxBytes: 4})
	tail.PushString("héllo")
	result = tail.Finish()
	if !utf8.ValidString(result.Text) || strings.ContainsRune(result.Text, 0xFFFD) {
		t.Fatalf("tail cut = %q", result.Text)
	}
	// A codepoint spanning the head|tail split with NO omission decodes as
	// one contiguous buffer: nothing dropped means no cut.
	span := NewTextRetainer(TextStrategy{Kind: "headTail", HeadBytes: 2, TailBytes: 2})
	span.PushString("é") // 2 bytes, fits both caps entirely
	result = span.Finish()
	if result.Text != "é" || result.Truncated {
		t.Fatalf("span = %q %+v", result.Text, result)
	}
}

func TestTextRetainerRollingSuffixBoundedMemory(t *testing.T) {
	retainer := NewTextRetainer(TextStrategy{Kind: "tail", MaxBytes: 4})
	for i := 0; i < 100; i++ {
		retainer.PushString("0123456789")
	}
	result := retainer.Finish()
	if result.Text != "6789" {
		t.Fatalf("rolling tail = %q", result.Text)
	}
	// Push decisions report per-chunk drops once the budget is exceeded.
	fresh := NewTextRetainer(TextStrategy{Kind: "head", MaxBytes: 3})
	if !fresh.PushString("abc").Kept {
		t.Fatal("first chunk should be kept")
	}
	if fresh.PushString("def").Kept != false || !fresh.PushString("def").Truncated {
		t.Fatal("second chunk dropped")
	}
}

func TestDescribeOmittedAndNotice(t *testing.T) {
	if got := DescribeOmitted(NoneOmitted(), "items"); got != "" {
		t.Fatalf("none = %q", got)
	}
	if got := DescribeOmitted(ExactOmitted(3), "items"); got != "Omitted 3 items." {
		t.Fatalf("exact = %q", got)
	}
	if got := DescribeOmitted(Omitted{Kind: OmittedUnknown}, "lines"); got != "More lines were omitted." {
		t.Fatalf("unknown = %q", got)
	}
	notice := RetentionNotice{Scope: "grep", Strategy: "head", Unit: "items", Limit: 10, Kept: 10, Omitted: ExactOmitted(7)}
	footer := FormatRetentionNotice(notice, func(n RetentionNotice) string {
		return "Narrow the pattern."
	})
	if footer != "Omitted 7 items. Narrow the pattern." {
		t.Fatalf("footer = %q", footer)
	}
	// Either half may be empty; joining adds no stray space.
	bare := FormatRetentionNotice(RetentionNotice{Omitted: NoneOmitted()}, func(RetentionNotice) string { return "" })
	if bare != "" {
		t.Fatalf("bare = %q", bare)
	}
}

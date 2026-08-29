package sessionquery

import (
	"strings"
	"unicode"
)

// cleanTitleText removes operating-system-command, control-sequence, and
// ESC escapes, control characters, and directional controls, then produces
// one trimmed, whitespace-normalized line.
//
// The official implementation expresses the OSC tail as a negative
// lookahead; Go's RE2 has none, so the escape sequences are consumed by a
// hand scanner with the same acceptance language.
func cleanTitleText(input string) string {
	var b strings.Builder
	runes := []rune(input)
	for index := 0; index < len(runes); index++ {
		r := runes[index]
		switch {
		case r == 0x1B && index+1 < len(runes) && runes[index+1] == ']':
			// OSC: ESC ] ... terminated by BEL or ESC \ (or unterminated tail).
			index = scanOSCTail(runes, index+2)
		case r == 0x9D: // 0x9D starts an OSC in C1.
			index = scanOSCTail(runes, index+1)
		case r == 0x1B && index+1 < len(runes) && runes[index+1] == '[':
			// CSI: ESC [ params [ -/ ]* final @-~ .
			index = scanCSI(runes, index+2)
		case r == 0x9B: // 0x9B starts a CSI in C1.
			index = scanCSI(runes, index+1)
		case r == 0x1B:
			// Remaining two-byte ESC sequences: ESC [@-_].
			if index+1 < len(runes) && runes[index+1] >= '@' && runes[index+1] <= '_' {
				index++
			}
		case r <= 0x1F && r != '\t' && r != '\n' && r != '\r' && r != '\v' && r != '\f',
			r == 0x7F,
			r >= 0x80 && r <= 0x9F,
			isDirectionalControl(r):
			// Non-whitespace C0/C1 control characters and directional
			// controls are dropped; whitespace collapses below.
		default:
			b.WriteRune(r)
		}
	}
	return collapseAndTrim(b.String())
}

// scanOSCTail consumes an OSC body through BEL, ESC \, or the end of input,
// returning the index of the final consumed rune.
func scanOSCTail(runes []rune, start int) int {
	for index := start; index < len(runes); index++ {
		if runes[index] == 0x07 {
			return index
		}
		if runes[index] == 0x1B && index+1 < len(runes) && runes[index+1] == '\\' {
			return index + 1
		}
	}
	return len(runes) - 1
}

// scanCSI consumes CSI parameters [0-?], intermediates [ -/], and the final
// byte [@-~], returning the index of the final consumed rune.
func scanCSI(runes []rune, start int) int {
	index := start
	for index < len(runes) && runes[index] >= '0' && runes[index] <= '?' {
		index++
	}
	for index < len(runes) && runes[index] >= ' ' && runes[index] <= '/' {
		index++
	}
	if index < len(runes) && runes[index] >= '@' && runes[index] <= '~' {
		return index
	}
	return index - 1
}

func isDirectionalControl(r rune) bool {
	switch {
	case r >= 0x200B && r <= 0x200F,
		r >= 0x202A && r <= 0x202E,
		r >= 0x2060 && r <= 0x2064,
		r >= 0x2066 && r <= 0x206F,
		r == 0xFEFF:
		return true
	}
	return false
}

func collapseAndTrim(input string) string {
	var b strings.Builder
	previousSpace := false
	for _, r := range input {
		if unicode.IsSpace(r) {
			previousSpace = true
			continue
		}
		if previousSpace && b.Len() > 0 {
			b.WriteByte(' ')
		}
		previousSpace = false
		b.WriteRune(r)
	}
	return b.String()
}

func trimEnd(input string) string {
	return strings.TrimRightFunc(input, func(r rune) bool {
		return unicode.IsSpace(r) || r == 0xFEFF
	})
}

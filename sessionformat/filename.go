package sessionformat

import "regexp"

// Canonical raw log basename shared by every generation-addressed Session
// artifact. Port of packages/session/session-format/src/filename.ts.

var canonicalLogFilename = regexp.MustCompile(`^session(?:\.v([1-9][0-9]*))?\.jsonl$`)

// SessionFormatLogFilename names the raw JSONL log of one immutable Session
// format generation. Version zero keeps the original `session.jsonl`; every
// later generation carries a lowercase numeric `.vN` component before the
// `.jsonl` suffix.
func SessionFormatLogFilename(version int64) (string, error) {
	if version < 0 {
		return "", formatErrorf("Session log generation version must be a non-negative safe integer")
	}
	if version == 0 {
		return "session.jsonl", nil
	}
	return "session.v" + itoa(version) + ".jsonl", nil
}

// ParseSessionFormatLogFilename reads the generation named by one raw JSONL
// log basename. Temporary, uppercase, leading-zero, `.v0`, and
// compression-suffixed names are not canonical. The second result is false
// when the name is not canonical.
func ParseSessionFormatLogFilename(filename string) (int64, bool) {
	match := canonicalLogFilename.FindStringSubmatch(filename)
	if match == nil {
		return 0, false
	}
	if match[1] == "" {
		return 0, true
	}
	var version int64
	for _, digit := range []byte(match[1]) {
		version = version*10 + int64(digit-'0')
		if version > 1<<53 {
			return 0, false
		}
	}
	return version, true
}

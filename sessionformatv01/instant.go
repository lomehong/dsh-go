package sessionformatv01

import "time"

// Canonical UTC instant validation mirroring the official regex + Date
// round-trip discipline. RE2 has no lookahead, so the 0000-year exclusion
// happens in code (the schedule package documents the same adaptation).

const instantLayout = "2006-01-02T15:04:05.000Z07:00"

func instantPatternMatches(value string) bool {
	if len(value) != 24 || !stringsHasSuffixZ(value) {
		return false
	}
	// (?!0000): year must not be 0000.
	if value[0] == '0' && value[1] == '0' && value[2] == '0' && value[3] == '0' {
		return false
	}
	return shapeMatches(value)
}

func stringsHasSuffixZ(value string) bool {
	return value[len(value)-1] == 'Z'
}

// shapeMatches checks the digit/separator skeleton the official regex
// pinned: \d{4}-d2-d2Td2:d2:d2.d3Z with calendar month/day and clock ranges.
func shapeMatches(value string) bool {
	digits := func(slice string) bool {
		for i := 0; i < len(slice); i++ {
			if slice[i] < '0' || slice[i] > '9' {
				return false
			}
		}
		return true
	}
	if !digits(value[0:4]) || value[4] != '-' || !digits(value[5:7]) || value[7] != '-' || !digits(value[8:10]) {
		return false
	}
	if value[10] != 'T' || !digits(value[11:13]) || value[13] != ':' || !digits(value[14:16]) || value[16] != ':' || !digits(value[17:19]) || value[19] != '.' || !digits(value[20:23]) {
		return false
	}
	month := int(value[5]-'0')*10 + int(value[6]-'0')
	day := int(value[8]-'0')*10 + int(value[9]-'0')
	if month < 1 || month > 12 {
		return false
	}
	if day < 1 || day > 31 {
		return false
	}
	hour := int(value[11]-'0')*10 + int(value[12]-'0')
	if hour > 23 {
		return false
	}
	minute := int(value[14]-'0')*10 + int(value[15]-'0')
	second := int(value[17]-'0')*10 + int(value[18]-'0')
	if minute > 59 || second > 59 {
		return false
	}
	return true
}

type parsedInstant struct{ value time.Time }

func parseInstant(value string) (parsedInstant, error) {
	parsed, err := time.Parse(instantLayout, value)
	if err != nil {
		return parsedInstant{}, err
	}
	return parsedInstant{parsed.UTC()}, nil
}

// equalCanonical reports whether the instant re-renders to exactly the same
// canonical text (the official toISOString round-trip check, which rejects
// impossible calendar days like 2026-02-30).
func (p parsedInstant) equalCanonical(original string) bool {
	return p.value.Format(instantLayout) == original
}

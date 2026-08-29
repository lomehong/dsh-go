package sessionquery

import (
	"math"
	"regexp"
	"strings"
)

// FilterSessionResults applies ANDed logical-session filters while
// preserving input order. List values within one clause are ORed; an empty
// list value matches nothing. Malformed clauses fail the whole call.
func FilterSessionResults(records []SessionRecord, filters []SessionResultFilter) ([]SessionRecord, error) {
	if len(filters) == 0 {
		return append([]SessionRecord(nil), records...), nil
	}
	predicates := make([]func(SessionRecord) bool, 0, len(filters))
	for _, filter := range filters {
		predicate, err := sessionPredicate(filter)
		if err != nil {
			return nil, err
		}
		predicates = append(predicates, predicate)
	}
	out := make([]SessionRecord, 0, len(records))
	for _, record := range records {
		accepted := true
		for _, predicate := range predicates {
			if !predicate(record) {
				accepted = false
				break
			}
		}
		if accepted {
			out = append(out, record)
		}
	}
	return out, nil
}

// FilterSessionEventDocuments applies ANDed event filters to extracted
// semantic documents, preserving input order. Malformed clauses fail the
// whole call.
func FilterSessionEventDocuments(documents []SessionEventSearchDocument, filters []SessionEventResultFilter) ([]SessionEventSearchDocument, error) {
	if len(filters) == 0 {
		return append([]SessionEventSearchDocument(nil), documents...), nil
	}
	predicates := make([]func(SessionEventSearchDocument) bool, 0, len(filters))
	for _, filter := range filters {
		predicate, err := eventPredicate(filter)
		if err != nil {
			return nil, err
		}
		predicates = append(predicates, predicate)
	}
	out := make([]SessionEventSearchDocument, 0, len(documents))
	for _, document := range documents {
		accepted := true
		for _, predicate := range predicates {
			if !predicate(document) {
				accepted = false
				break
			}
		}
		if accepted {
			out = append(out, document)
		}
	}
	return out, nil
}

// MaterializeSessionResultFilters copies and validates logical-session
// filters before an asynchronous boundary, returning detached clauses.
func MaterializeSessionResultFilters(filters []SessionResultFilter) ([]SessionResultFilter, error) {
	out := make([]SessionResultFilter, 0, len(filters))
	for _, filter := range filters {
		copied := SessionResultFilter{Kind: filter.Kind}
		switch filter.Kind {
		case "id", "availability":
			values, err := copyStrings(filter.Kind, filter.Values)
			if err != nil {
				return nil, err
			}
			if filter.Kind == "availability" {
				if err := assertAllowedValues(filter.Kind, values, []string{AvailabilityLive, AvailabilityPersisted}); err != nil {
					return nil, err
				}
			}
			copied.Values = values
		case "cwd", "parent":
			values, err := copyNullableStrings(filter.Kind, filter.NullableValues)
			if err != nil {
				return nil, err
			}
			copied.NullableValues = values
		case "created-at":
			if err := validateRange(filter.Kind, filter.From, filter.To); err != nil {
				return nil, err
			}
			copied.From, copied.To = filter.From, filter.To
		default:
			return nil, unknownFilter(filter.Kind)
		}
		out = append(out, copied)
	}
	return out, nil
}

// MaterializeSessionEventResultFilters copies and validates event filters
// before an asynchronous boundary, returning detached clauses.
func MaterializeSessionEventResultFilters(filters []SessionEventResultFilter) ([]SessionEventResultFilter, error) {
	out := make([]SessionEventResultFilter, 0, len(filters))
	for _, filter := range filters {
		copied := SessionEventResultFilter{Kind: filter.Kind}
		switch filter.Kind {
		case "seq", "time":
			if err := validateRange(filter.Kind, filter.From, filter.To); err != nil {
				return nil, err
			}
			copied.From, copied.To = filter.From, filter.To
		case "type":
			values, err := copyStrings(filter.Kind, filter.Values)
			if err != nil {
				return nil, err
			}
			copied.Values = values
		case "surface":
			values, err := copyStrings(filter.Kind, filter.Values)
			if err != nil {
				return nil, err
			}
			if err := assertAllowedValues(filter.Kind, values, []string{SurfaceCurrent, SurfaceShadowed, SurfaceLogOnly}); err != nil {
				return nil, err
			}
			copied.Values = values
		case "text":
			// The text clause is a plain string in the typed surface; the
			// copy keeps the detach contract.
			copied.Text = filter.Text
		default:
			return nil, unknownFilter(filter.Kind)
		}
		out = append(out, copied)
	}
	return out, nil
}

// CompileSessionTextFilter compiles a literal case-insensitive,
// whitespace-flexible semantic-text match: a regular expression safe from
// regex injection.
func CompileSessionTextFilter(text string) (*regexp.Regexp, error) {
	trimmed := strings.TrimSpace(text)
	if len(trimmed) == 0 {
		return nil, queryError(CodeInvalidFilter, "session text filter must contain non-whitespace text")
	}
	separator := regexp.MustCompile(`\s+`)
	parts := separator.Split(trimmed, -1)
	escaped := make([]string, 0, len(parts))
	for _, part := range parts {
		escaped = append(escaped, regexpQuoteMeta(part))
	}
	pattern := "(?i)" + strings.Join(escaped, `\s+`)
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		return nil, queryError(CodeInvalidFilter, "session text filter %q is not compilable: %v", text, err)
	}
	return compiled, nil
}

// regexpQuoteMeta escapes the same metacharacter set as the official
// literal escape: .*+?^${}()|[]\
func regexpQuoteMeta(part string) string {
	var b strings.Builder
	for _, r := range part {
		switch r {
		case '.', '*', '+', '?', '^', '$', '{', '}', '(', ')', '|', '[', ']', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func sessionPredicate(filter SessionResultFilter) (func(SessionRecord) bool, error) {
	switch filter.Kind {
	case "id":
		return func(record SessionRecord) bool { return containsString(filter.Values, record.Header.ID) }, nil
	case "cwd":
		return func(record SessionRecord) bool {
			var cwd *string
			if record.Header.CWD != "" {
				v := record.Header.CWD
				cwd = &v
			}
			return containsNullableString(filter.NullableValues, cwd)
		}, nil
	case "created-at":
		return func(record SessionRecord) bool {
			return matchesRange(float64(record.Header.CreatedAt), filter.From, filter.To)
		}, nil
	case "parent":
		return func(record SessionRecord) bool {
			var parent *string
			if record.Header.ParentSession != "" {
				v := record.Header.ParentSession
				parent = &v
			}
			return containsNullableString(filter.NullableValues, parent)
		}, nil
	case "availability":
		if err := assertAllowedValues(filter.Kind, filter.Values, []string{AvailabilityLive, AvailabilityPersisted}); err != nil {
			return nil, err
		}
		return func(record SessionRecord) bool {
			for _, value := range filter.Values {
				if value == AvailabilityLive && record.Live {
					return true
				}
				if value == AvailabilityPersisted && record.Persisted {
					return true
				}
			}
			return false
		}, nil
	default:
		return nil, unknownFilter(filter.Kind)
	}
}

func eventPredicate(filter SessionEventResultFilter) (func(SessionEventSearchDocument) bool, error) {
	switch filter.Kind {
	case "seq":
		return func(d SessionEventSearchDocument) bool { return matchesRange(float64(d.Seq), filter.From, filter.To) }, nil
	case "time":
		return func(d SessionEventSearchDocument) bool { return matchesRange(float64(d.Time), filter.From, filter.To) }, nil
	case "type":
		return func(d SessionEventSearchDocument) bool { return containsString(filter.Values, d.Type) }, nil
	case "surface":
		if err := assertAllowedValues(filter.Kind, filter.Values, []string{SurfaceCurrent, SurfaceShadowed, SurfaceLogOnly}); err != nil {
			return nil, err
		}
		return func(d SessionEventSearchDocument) bool { return containsString(filter.Values, d.Surface) }, nil
	case "text":
		pattern, err := CompileSessionTextFilter(filter.Text)
		if err != nil {
			return nil, err
		}
		return func(d SessionEventSearchDocument) bool { return pattern.MatchString(d.Text) }, nil
	default:
		return nil, unknownFilter(filter.Kind)
	}
}

func copyStrings(kind string, values []string) ([]string, error) {
	out := make([]string, len(values))
	copy(out, values)
	return out, nil
}

func copyNullableStrings(kind string, values []*string) ([]*string, error) {
	out := make([]*string, len(values))
	copy(out, values)
	return out, nil
}

func unknownFilter(kind string) error {
	if kind == "" {
		return queryError(CodeInvalidFilter, "session unknown filter kind (missing)")
	}
	return queryError(CodeInvalidFilter, "session unknown filter kind %q", kind)
}

func assertAllowedValues(kind string, values, allowed []string) error {
	for _, value := range values {
		if !containsString(allowed, value) {
			return queryError(CodeInvalidFilter, "session %s filter contains unknown value %q", kind, value)
		}
	}
	return nil
}

func validateRange(kind string, from, to *float64) error {
	if from != nil && (math.IsNaN(*from) || math.IsInf(*from, 0)) {
		return queryError(CodeInvalidFilter, "session %s filter from must be finite", kind)
	}
	if to != nil && (math.IsNaN(*to) || math.IsInf(*to, 0)) {
		return queryError(CodeInvalidFilter, "session %s filter to must be finite", kind)
	}
	if from != nil && to != nil && *from > *to {
		return queryError(CodeInvalidFilter, "session %s filter from must be less than or equal to to", kind)
	}
	return nil
}

func matchesRange(value float64, from, to *float64) bool {
	if from != nil && value < *from {
		return false
	}
	if to != nil && value > *to {
		return false
	}
	return true
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsNullableString(values []*string, want *string) bool {
	for _, value := range values {
		if value == nil && want == nil {
			return true
		}
		if value != nil && want != nil && *value == *want {
			return true
		}
	}
	return false
}

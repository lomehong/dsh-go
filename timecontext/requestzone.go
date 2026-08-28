package timecontext

import (
	"fmt"
	"regexp"
	"sort"

	"dshgo/llm"
)

// ianaTimeZone matches the Host-canonicalized browser zone shape: canonical
// UTC or an IANA Area/Location path.
var ianaTimeZone = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+.-]*(?:/[A-Za-z0-9_+.-]+)+$`)

// BrowserTimeZoneContext is the browser-zone facts derived from user-rpc
// messages in one open turn. Kind is "resolved", "mixed", or "missing".
type BrowserTimeZoneContext struct {
	Kind string
	// TimeZone is the unique resolved zone (kind "resolved").
	TimeZone string
	// TimeZones is the sorted, duplicate-free zone set (kind "mixed").
	TimeZones []string
}

// browserTimeZoneOf reads and validates the browser zone from one ordinary
// user-rpc message: "" when the message carries no user-rpc source; an
// error when a user-rpc source carries an invalid or noncanonical zone.
// Go adaptation: the source's Intl canonicalization round-trip collapses to
// the IANA shape check plus LoadLocation success (both Host
// canonicalization duties).
func browserTimeZoneOf(message llm.Message) (string, error) {
	source := message.Source
	if source.Kind != llm.SourceUser || source.RPCID == "" || source.ClientTimeZone == "" {
		return "", nil
	}
	value := source.ClientTimeZone
	if value != "UTC" && !ianaTimeZone.MatchString(value) {
		return "", fmt.Errorf("browser time zone must be canonical UTC or IANA Area/Location: %q", value)
	}
	if _, err := CreateTimestampFormatter(value); err != nil {
		return "", fmt.Errorf("browser time zone is unsupported: %q", value)
	}
	return value, nil
}

// DeriveBrowserTimeZoneContext derives the unique, mixed, or missing
// browser zone for one open turn from the entered and proposed user
// messages belonging to it.
func DeriveBrowserTimeZoneContext(messages []llm.Message) (BrowserTimeZoneContext, error) {
	seen := map[string]struct{}{}
	for _, message := range messages {
		timeZone, err := browserTimeZoneOf(message)
		if err != nil {
			return BrowserTimeZoneContext{}, err
		}
		if timeZone != "" {
			seen[timeZone] = struct{}{}
		}
	}
	timeZones := make([]string, 0, len(seen))
	for timeZone := range seen {
		timeZones = append(timeZones, timeZone)
	}
	sort.Strings(timeZones)
	if len(timeZones) == 0 {
		return BrowserTimeZoneContext{Kind: "missing"}, nil
	}
	if len(timeZones) == 1 {
		return BrowserTimeZoneContext{Kind: "resolved", TimeZone: timeZones[0]}, nil
	}
	return BrowserTimeZoneContext{Kind: "mixed", TimeZones: timeZones}, nil
}

// RenderBrowserTimeZoneContext renders the model instruction for one
// browser-zone context: one durable policy line.
func RenderBrowserTimeZoneContext(context BrowserTimeZoneContext) string {
	switch context.Kind {
	case "resolved":
		return fmt.Sprintf("Browser time zone for this request: %s. "+
			"Interpret otherwise-unqualified dates and times in this zone.", context.TimeZone)
	case "mixed":
		return fmt.Sprintf("Browser time zone for this request: mixed %s. "+
			"Ask the user to clarify otherwise-unqualified dates and times.", jsonArray(context.TimeZones))
	default:
		return "Browser time zone for this request: unavailable. " +
			"Ask the user to clarify otherwise-unqualified dates and times."
	}
}

// jsonArray renders the source's JSON.stringify of the zone list.
func jsonArray(values []string) string {
	encoded, _ := jsonMarshal(values)
	return encoded
}

// Package timecontext ports packages/context/time-context: opt-in request
// clock context. Eligible steps add durable, source-attributed time
// readings to the request history as plugin-sourced snapshot user messages.
package timecontext

import (
	"fmt"
	"time"
)

// CreateTimestampFormatter resolves the display zone: an explicit IANA
// name, or the process fallback when empty. Go adaptation: Intl
// canonicalization collapses to time.LoadLocation success; the canonical
// label carried in brackets is the caller's zone string (the source's
// resolvedOptions().timeZone round-trip).
func CreateTimestampFormatter(timeZone string) (*time.Location, error) {
	if timeZone == "" {
		return time.Local, nil
	}
	return time.LoadLocation(timeZone)
}

// FormatTimestamp renders an epoch millisecond value as an ISO-shaped
// timestamp with offset and IANA zone:
// `2026-01-02T03:04:05+08:00[Asia/Shanghai]`. The offset is the long
// numeric form (UTC renders `+00:00`, never `Z`), matching the source's
// longOffset part with the GMT+00:00 rewrite.
func FormatTimestamp(now int64, loc *time.Location, timeZone string) string {
	stamp := time.UnixMilli(now).In(loc)
	_, offsetSeconds := stamp.Zone()
	offset := renderLongOffset(offsetSeconds)
	return fmt.Sprintf("%s%s[%s]",
		stamp.Format("2006-01-02T15:04:05"), offset, timeZone)
}

// renderLongOffset renders a zone offset in the `+HH:MM` long form.
func renderLongOffset(offsetSeconds int) string {
	sign := "+"
	if offsetSeconds < 0 {
		sign = "-"
		offsetSeconds = -offsetSeconds
	}
	hours := offsetSeconds / 3600
	minutes := (offsetSeconds % 3600) / 60
	return fmt.Sprintf("%s%02d:%02d", sign, hours, minutes)
}

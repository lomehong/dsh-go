// Coordinator helpers: JSON canonicity for seed comparison and duration
// conversion.
package persistence

import (
	"encoding/json"
	"time"

	"dshgo/session"
)

// eventJSON renders one event through its canonical wire form (the same
// encoding the log uses), so seed comparison is byte-exact on the official
// JSON.stringify comparison semantics.
func eventJSON(event session.Event) string {
	raw, err := json.Marshal(event)
	if err != nil {
		// Events in memory are already JSON-validated; failure here cannot
		// produce a match either way.
		return "\x00unmarshalable"
	}
	return string(raw)
}

func seedJSON(event session.Event) (string, error) {
	raw, err := json.Marshal(event)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// msToDuration converts a millisecond policy value.
func msToDuration(ms int64) time.Duration { return time.Duration(ms) * time.Millisecond }

package sessionstats

import "encoding/json"

// DecodeStateForTest exposes decodeState to the package tests.
func DecodeStateForTest(t interface{ Fatalf(string, ...any) }, raw string) (any, error) {
	return decodeState(json.RawMessage(raw))
}

package sessionstats

import "encoding/json"

// DecodeStateForTest exposes decodeState to the package tests.
func DecodeStateForTest(t interface{ Fatalf(string, ...any) }, raw string) (*State, error) {
	decoded, err := decodeState(json.RawMessage(raw))
	if err != nil {
		return nil, err
	}
	return decoded, nil
}

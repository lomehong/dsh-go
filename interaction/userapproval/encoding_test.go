package userapproval

import "encoding/json"

// unmarshalForTest decodes one event payload; shared by tests.
func unmarshalForTest(data []byte, target any) error {
	return json.Unmarshal(data, target)
}

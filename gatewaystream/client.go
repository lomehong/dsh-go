package gatewaystream

import (
	"encoding/json"
	"fmt"
)

// clientMessage is one validated client frame over the Remote mux.
type clientMessage struct {
	Kind     string
	StreamID string
	Endpoint string
	Payload  any
}

// parseClientMessage validates one untrusted client message.
func parseClientMessage(raw []byte) (clientMessage, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return clientMessage{}, fmt.Errorf("api gateway: invalid Remote stream client message")
	}
	record, ok := value.(map[string]any)
	if !ok {
		return clientMessage{}, fmt.Errorf("api gateway: invalid Remote stream client message")
	}
	kind, _ := record["type"].(string)
	streamID, _ := record["streamId"].(string)
	switch kind {
	case "open":
		endpoint, ok := record["endpoint"].(string)
		if !ok || endpoint == "" {
			return clientMessage{}, fmt.Errorf("api gateway: invalid Remote stream open message")
		}
		payload, hasPayload := record["payload"]
		if hasPayload && !isRemoteJSONValue(payload) {
			return clientMessage{}, fmt.Errorf("api gateway: Remote stream payload is not lossless JSON")
		}
		return clientMessage{Kind: "open", StreamID: streamID, Endpoint: endpoint, Payload: payload}, nil
	case "cancel":
		return clientMessage{Kind: "cancel", StreamID: streamID}, nil
	default:
		return clientMessage{}, fmt.Errorf("api gateway: unknown Remote stream client message kind")
	}
}

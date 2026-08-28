package subagent

import (
	"dshgo/session"
)

// SeedDescriptorTurn builds the child's creation seed: any inherited
// parent-history prefix followed by one model-hidden, between-turn
// `descriptor` event. Staging through a Session assigns the sequence number
// and enforces the same lossless-JSON rules the durable log does. The
// returned events are contiguous from sequence zero.
func SeedDescriptorTurn(childID session.SessionID, seed []session.Event, descriptor SubagentDescriptorData) ([]session.Event, error) {
	staged, err := session.NewDetached(childID, seed, &session.SessionHeader{ID: childID})
	if err != nil {
		return nil, err
	}
	if _, err := staged.Append(EventSubagentDescriptor, descriptor, nil); err != nil {
		return nil, err
	}
	return staged.Events(), nil
}

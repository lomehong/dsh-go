package gateway

import (
	"context"
)

// EmptyRemoteEventSource is the honest forwarded-event source while the Go
// host composes no allowlisted Host event emitters: the iterator ends
// immediately, so every $events stream opens (ready frame) and stays open
// with no events — the empty-state contract the browser readiness gate
// consumes.
func EmptyRemoteEventSource(signal context.Context) RemoteEventDispatchIter {
	return emptyDispatchIter{}
}

type emptyDispatchIter struct{}

func (emptyDispatchIter) Next() (RemoteEventDispatch, bool) {
	return RemoteEventDispatch{}, false
}

func (emptyDispatchIter) Dispose() {}

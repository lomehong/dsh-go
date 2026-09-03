package gateway

import (
	"context"
)

// sessionControlEndpoint is the official session control stream the browser
// opens over the mux (api-session-controller client: RemoteSnapshotStream →
// remote.session.control(signal)).
const sessionControlEndpoint = "session/control"

// openSessionControl answers one session control stream: an opening baseline
// frame with the empty queue/job/projection maps, then the stream stays open
// until the caller's signal ends or is cancelled. The Go host runs no live
// session queueing yet, so the honest baseline is empty; keeping the stream
// open satisfies the client's snapshot lifecycle (a closed stream before the
// baseline would surface as "ended before its opening snapshot").
func (g *Gateway) openSessionControl(args map[string]any, signal context.Context) (<-chan any, func(), error) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer cancel()
		select {
		case <-signal.Done():
		case <-ctx.Done():
		}
	}()
	frames := make(chan any)
	go func() {
		defer close(frames)
		baseline := map[string]any{
			"type": "baseline",
			"value": map[string]any{
				"queues":      map[string]any{},
				"jobs":        map[string]any{},
				"projections": map[string]any{},
			},
		}
		select {
		case frames <- baseline:
		case <-signal.Done():
			return
		case <-ctx.Done():
			return
		}
		select {
		case <-signal.Done():
		case <-ctx.Done():
		}
	}()
	return frames, cancel, nil
}

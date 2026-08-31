// RegistryProjectionSource adapts the projection registry to the optional
// ProjectionSource seam used by session observations: SnapshotLive mirrors a
// live session's registry state; HydratePrepared folds one prepared log via
// the registry's restore-then-hydrate path. Both are optional capabilities
// the composition wires.
package sessionquery

import (
	"dshgo/session"
	"dshgo/session/projection"
)

// RegistryProjectionSource bridges the projection registry onto the
// observation seam.
type RegistryProjectionSource struct {
	Registry *projection.Registry
}

// SnapshotLive computes every registered projection for a live session.
func (s RegistryProjectionSource) SnapshotLive(sess *session.Session) (projection.Snapshot, bool) {
	if s.Registry == nil || sess == nil {
		return projection.Snapshot{}, false
	}
	snapshot := s.Registry.Snapshot(sess)
	return snapshot, len(snapshot.Values) > 0
}

// HydratePrepared folds one prepared session's exact log through the
// registry's restore path and installs the resulting cells.
func (s RegistryProjectionSource) HydratePrepared(sess *session.Session, meta session.SessionHeader, events []session.Event) (projection.Snapshot, bool, error) {
	if s.Registry == nil || sess == nil {
		return projection.Snapshot{}, false, nil
	}
	snapshot, err := s.Registry.Hydrate(sess, projection.Checkpoint{}, events, 0)
	if err != nil {
		return projection.Snapshot{}, false, err
	}
	return snapshot, true, nil
}

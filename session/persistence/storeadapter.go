// The store adapter: one live session registry shaped for the coordinator
// (the official `ctx.sessions` consumer seam). The store's own Get/List
// return id-shaped results; the coordinator wants session-shaped ones with
// presence flags, plus Prepare building the exact unpublished session for
// resume from a seed.
package persistence

import "dshgo/session"

// StoreSessions adapts a *session.Store to the Sessions seam.
type StoreSessions struct {
	// Store is the live session registry.
	Store *session.Store
}

// NewSessionsAdapter wraps the store as the coordinator's registry view.
func NewSessionsAdapter(store *session.Store) *StoreSessions {
	return &StoreSessions{Store: store}
}

// Get resolves one live session by id.
func (a *StoreSessions) Get(id session.SessionID) (*session.Session, bool) {
	s := a.Store.Get(id)
	return s, s != nil
}

// List enumerates live sessions (HMR re-seed), skipping races where a
// session left the store between enumeration and resolution.
func (a *StoreSessions) List() []*session.Session {
	ids := a.Store.List()
	out := make([]*session.Session, 0, len(ids))
	for _, id := range ids {
		if s := a.Store.Get(id); s != nil {
			out = append(out, s)
		}
	}
	return out
}

// Prepare builds the exact unpublished Session for resume: a restored
// session over the seed events, not yet announced to the store.
func (a *StoreSessions) Prepare(id session.SessionID, seed []session.Event, meta session.SessionHeader) (*session.Session, error) {
	return session.NewRestored(id, seed, meta)
}

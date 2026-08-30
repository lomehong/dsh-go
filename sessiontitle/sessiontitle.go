// The session-title service: the explicit rename surface and the log-backed
// read face.
//
// Port of SessionTitleService.rename/get in
// packages/session/session-title/src/index.ts. The asynchronous title
// family — provider registration, automatic scheduling, fallback
// generation, and their supersession races — stays deferred (recorded in
// the roadmap as the title family); rename is the surface the webhook
// session transaction needs, and it is ported in full: liveness check,
// normalization, the invalid-title refusal, the durable session/title
// event with source `user`, and the fold back to the accepted snapshot.
package sessiontitle

import (
	"fmt"

	"dshgo/session"
	"dshgo/sessionquery"
)

// EventTitle is the durable log-only event one accepted title writes.
const EventTitle = "session/title"

// Config carries the service-owned limits. All three are required, exactly
// like the official schema; the shipped base composition uses 5/40/80.
type Config struct {
	// FallbackMaxWords caps the built-in first-prompt fallback.
	FallbackMaxWords int `json:"fallbackMaxWords"`
	// FallbackMaxBytes caps the built-in fallback's UTF-8 size.
	FallbackMaxBytes int `json:"fallbackMaxBytes"`
	// MaxTitleBytes caps every accepted title's UTF-8 size.
	MaxTitleBytes int `json:"maxTitleBytes"`
}

// SessionTitleInvalidError: the proposed title normalizes to nothing.
type SessionTitleInvalidError struct {
	Message string
}

func (e *SessionTitleInvalidError) Error() string { return e.Message }

// Service is one deployment's title surface over the live session store.
type Service struct {
	store  *session.Store
	config Config
}

// NewService builds the title service over the session store. The limits
// are validated exactly like the official schema: every bound positive,
// the fallback budget never above the title budget.
func NewService(store *session.Store, config Config) (*Service, error) {
	if store == nil {
		return nil, fmt.Errorf("session-title: the sessions store is required")
	}
	if config.FallbackMaxWords < 1 {
		return nil, fmt.Errorf("session-title: fallbackMaxWords must be a positive integer")
	}
	if config.FallbackMaxBytes < 1 {
		return nil, fmt.Errorf("session-title: fallbackMaxBytes must be a positive integer")
	}
	if config.MaxTitleBytes < 1 {
		return nil, fmt.Errorf("session-title: maxTitleBytes must be a positive integer")
	}
	if config.FallbackMaxBytes > config.MaxTitleBytes {
		return nil, fmt.Errorf("session-title: fallbackMaxBytes must not exceed maxTitleBytes")
	}
	return &Service{store: store, config: config}, nil
}

// Rename pins one explicit title on a live session: normalize, refuse an
// empty result, append the durable session/title event with source `user`,
// and fold back the accepted snapshot. Port of rename in index.ts; the
// refusal wordings are verbatim.
//
// The official rename also supersedes any in-flight automatic generation;
// the Go port defers the automatic family, so there is nothing to
// supersede yet — the pin itself (latest logged title wins) already holds.
func (s *Service) Rename(sess *session.Session, title string) (*sessionquery.SessionTitleSnapshot, error) {
	if s.store.Get(sess.ID()) != sess {
		return nil, fmt.Errorf("session %q is not live in this store", sess.ID())
	}
	normalized := sessionquery.NormalizeSessionTitle(title, s.config.MaxTitleBytes)
	if len(normalized) == 0 {
		return nil, &SessionTitleInvalidError{Message: "session title must contain visible characters"}
	}
	if _, err := sess.Append(EventTitle, sessionquery.SessionTitleEventData{
		Title:       normalized,
		MessageSeqs: []int64{},
		Source:      sessionquery.SessionTitleSource{Kind: sessionquery.TitleSourceUser},
	}, nil); err != nil {
		return nil, err
	}
	snapshot := s.Get(sess)
	if snapshot == nil {
		return nil, fmt.Errorf("renamed title failed to fold")
	}
	return snapshot, nil
}

// Get folds the latest accepted title, or nil when none is logged.
func (s *Service) Get(sess *session.Session) *sessionquery.SessionTitleSnapshot {
	return sessionquery.FoldSessionTitle(sess.Events())
}

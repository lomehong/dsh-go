// Package sessionlog ports @deepseek-ai/dsh-session-log-deepseek: the
// incremental session-log contribution for official DeepSeek LLM API
// requests. Accepted sequence watermarks live in the canonical log, so
// restart recovery can conservatively resend uncertain tails without
// maintaining another store.
//
// Go adaptation: the official WeakMap fold cache becomes an explicit Folder
// owned by the host (no weak references), and the deepseek request-extension
// registry is a package-local seam awaiting the Go adapter extension point.
package sessionlog

import (
	"encoding/json"
	"fmt"
	"sync"

	"dshgo/session"
)

// Name is the cordis plugin name.
const Name = "session-log-deepseek"

// EventTypeDeliveryAccepted is the durable acceptance watermark event: the
// extension appends it once a request carrying the contribution is accepted.
const EventTypeDeliveryAccepted = "session-log-deepseek/delivery-accepted"

// DeliveryAcceptedData is the watermark event payload.
type DeliveryAcceptedData struct {
	// SessionID is the exact session identity the watermark belongs to.
	SessionID string `json:"sessionId"`
	// ThroughSeq is the highest accepted event sequence.
	ThroughSeq int64 `json:"throughSeq"`
}

// decodeDeliveryAccepted decodes one watermark event, reporting whether the
// event type matches. A matching event with a malformed payload is an error:
// the watermark drives resend decisions, so a corrupt one must fail loud.
func decodeDeliveryAccepted(e session.Event) (DeliveryAcceptedData, error, bool) {
	if e.Type != EventTypeDeliveryAccepted {
		return DeliveryAcceptedData{}, nil, false
	}
	var data DeliveryAcceptedData
	if err := json.Unmarshal(e.Data, &data); err != nil {
		return DeliveryAcceptedData{}, fmt.Errorf("session-log-deepseek: malformed acceptance watermark at seq %d", e.Seq), true
	}
	if data.SessionID == "" || data.ThroughSeq < 0 || data.ThroughSeq >= e.Seq {
		return DeliveryAcceptedData{}, fmt.Errorf("session-log-deepseek: malformed acceptance watermark at seq %d", e.Seq), true
	}
	return data, nil, true
}

// acceptanceFold is the incremental scan position for one session.
type acceptanceFold struct {
	scannedEvents int
	throughSeq    int64
	seen          bool
}

// Folder folds acceptance watermarks incrementally per session. The host
// owns one Folder for the process lifetime; it is safe for concurrent use.
type Folder struct {
	mu    sync.Mutex
	folds map[*session.Session]acceptanceFold
}

// NewFolder builds an empty fold cache.
func NewFolder() *Folder {
	return &Folder{folds: map[*session.Session]acceptanceFold{}}
}

// AcceptedThrough returns the highest confirmed sequence for this exact
// session identity — the greatest accepted watermark, or -1 before any
// accepted request. Only events appended since the previous fold are
// rescanned.
func (f *Folder) AcceptedThrough(sess *session.Session) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	previous, seenSession := f.folds[sess]
	throughSeq := int64(-1)
	if seenSession && previous.seen {
		throughSeq = previous.throughSeq
	}
	events := sess.Events()
	for index := previous.scannedEvents; index < len(events); index++ {
		event := events[index]
		data, err, matched := decodeDeliveryAccepted(event)
		if err != nil {
			return 0, err
		}
		if !matched || data.SessionID != string(sess.ID()) {
			continue
		}
		if data.ThroughSeq > throughSeq {
			throughSeq = data.ThroughSeq
		}
	}
	f.folds[sess] = acceptanceFold{scannedEvents: len(events), throughSeq: throughSeq, seen: true}
	return throughSeq, nil
}

// Contribution is the dsh_session_log request value: the canonical events
// the receiver has not yet confirmed, bracketed by the watermark interval.
type Contribution struct {
	// Version is the contribution wire format.
	Version int `json:"version"`
	// Session is the producing session's canonical header.
	Session session.SessionHeader `json:"session"`
	// AfterSeq is the previously accepted watermark (-1 before any).
	AfterSeq int64 `json:"afterSeq"`
	// ThroughSeq is the last event sequence this contribution carries.
	ThroughSeq int64 `json:"throughSeq"`
	// Events is the suffix after AfterSeq, verbatim.
	Events []session.Event `json:"events"`
}

// PreparedContribution is one prepared request field: the wire value plus
// the acceptance callback the adapter invokes once the request is accepted.
type PreparedContribution struct {
	Value Contribution
	// Accept records the watermark durably. The delivery is only then
	// considered confirmed; a crash before Accept conservatively resends.
	Accept func() error
}

// Prepare builds the contribution for one request against its session, or
// returns nil when there is nothing to send (an empty log). The folder
// supplies the previously accepted watermark.
func Prepare(folder *Folder, sess *session.Session) (*PreparedContribution, error) {
	afterSeq, err := folder.AcceptedThrough(sess)
	if err != nil {
		return nil, err
	}
	snapshot := sess.Events()
	throughSeq := int64(len(snapshot)) - 1
	if throughSeq < 0 {
		return nil, nil
	}
	suffix := append([]session.Event{}, snapshot[afterSeq+1:]...)
	contribution := Contribution{
		Version:    1,
		Session:    sess.Header(),
		AfterSeq:   afterSeq,
		ThroughSeq: throughSeq,
		Events:     suffix,
	}
	return &PreparedContribution{
		Value: contribution,
		Accept: func() error {
			_, err := sess.Append(EventTypeDeliveryAccepted, DeliveryAcceptedData{
				SessionID:  string(sess.ID()),
				ThroughSeq: throughSeq,
			}, nil)
			return err
		},
	}, nil
}

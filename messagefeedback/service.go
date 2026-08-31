// Package messagefeedback ports packages/feedback/message-feedback: durable,
// lifecycle-bound feedback for finalized assistant messages. It inspects
// persisted Session history and never creates or resumes an Agent or
// Session — the storage-domain sidecar keeps absence inside the business
// union.
package messagefeedback

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"dshgo/llm"
	"dshgo/session"
	"dshgo/session/persistence"
	"dshgo/storagedomain"
)

// Rating is the closed rating vocabulary.
type Rating string

const (
	RatingPositive Rating = "positive"
	RatingNegative Rating = "negative"
)

// Item is one feedback item for a finalized assistant message.
type Item struct {
	MessageID string  `json:"messageId"`
	Rating    Rating  `json:"rating"`
	Note      *string `json:"note,omitempty"`
	Version   string  `json:"version"`
	CreatedAt int64   `json:"createdAt"`
	UpdatedAt int64   `json:"updatedAt"`
}

// SessionIdentity is the persisted Session fields that fence a sidecar row
// to one log lifecycle.
type SessionIdentity struct {
	CreatedAt int64   `json:"createdAt"`
	CWD       *string `json:"cwd,omitempty"`
}

// Row is one whole-Session sidecar.
type Row struct {
	Session SessionIdentity `json:"session"`
	Items   []Item          `json:"items"`
}

// Config is the deployment-owned policy.
type Config struct {
	// MaxNoteBytes is the maximum UTF-8 byte length accepted for one note.
	MaxNoteBytes int64
}

// Spec is the storage-domain declaration.
func Spec() storagedomain.DomainSpec {
	return storagedomain.DomainSpec{
		Name:    "message_feedback",
		Version: 1,
		Tables:  []string{"sessions"},
		ValidateRecord: func(table, key string, raw json.RawMessage) error {
			if table != "sessions" {
				return fmt.Errorf("message-feedback: unknown table %q", table)
			}
			return validateRow(raw)
		},
	}
}

// validateRow enforces the item schema at the durable read boundary.
func validateRow(raw json.RawMessage) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var row Row
	if err := decoder.Decode(&row); err != nil {
		return fmt.Errorf("message-feedback row: %w", err)
	}
	if row.Session.CreatedAt < 0 {
		return fmt.Errorf("message-feedback row session.createdAt must be non-negative")
	}
	messageIDs := map[string]struct{}{}
	versions := map[string]struct{}{}
	for _, item := range row.Items {
		if item.MessageID == "" {
			return fmt.Errorf("message-feedback item messageId must be non-empty")
		}
		if item.Rating != RatingPositive && item.Rating != RatingNegative {
			return fmt.Errorf("message-feedback item rating is invalid")
		}
		if item.Note != nil && strings.TrimSpace(*item.Note) == "" {
			return fmt.Errorf("message-feedback note must contain a non-whitespace character")
		}
		if item.Version == "" {
			return fmt.Errorf("message-feedback item version must be non-empty")
		}
		if item.CreatedAt < 0 || item.UpdatedAt < 0 || item.UpdatedAt < item.CreatedAt {
			return fmt.Errorf("message-feedback item timestamps must be ordered non-negative")
		}
		if _, dup := messageIDs[item.MessageID]; dup {
			return fmt.Errorf("message-feedback duplicate messageId %q", item.MessageID)
		}
		if _, dup := versions[item.Version]; dup {
			return fmt.Errorf("message-feedback duplicate version %q", item.Version)
		}
		messageIDs[item.MessageID] = struct{}{}
		versions[item.Version] = struct{}{}
	}
	return nil
}

// FailureCode is one explicit business failure branch.
type FailureCode string

const (
	FailureSessionNotFound FailureCode = "session-not-found"
	FailureTargetNotFound  FailureCode = "target-not-found"
	FailureVersionConflict FailureCode = "version-conflict"
	FailureNoteBlank       FailureCode = "note-blank"
	FailureNoteTooLarge    FailureCode = "note-too-large"
)

// Failure is one explicit business failure payload.
type Failure struct {
	Code        FailureCode        `json:"code"`
	SessionID   *session.SessionID `json:"sessionId,omitempty"`
	MessageID   *string            `json:"messageId,omitempty"`
	MaxBytes    *int64             `json:"maxBytes,omitempty"`
	ActualBytes *int64             `json:"actualBytes,omitempty"`
	Current     *Item              `json:"current,omitempty"`
}

// Result is the closed success/failure union for one call.
type Result struct {
	OK    bool
	Value any
	Error *Failure
}

func success(value any) Result   { return Result{OK: true, Value: value} }
func rejected(f *Failure) Result { return Result{OK: false, Error: f} }

// Service is the storage-domain sidecar for message feedback.
type Service struct {
	maxNoteBytes int64
	domain       *storagedomain.Domain
	table        storagedomain.Table
	coordinator  *persistence.Coordinator
	store        *session.Store

	mu            sync.Mutex
	opTails       map[session.SessionID]chan struct{}
	admissionOpen bool
}

// New validates the config at the configuration boundary.
func New(cfg Config) (*Service, error) {
	if cfg.MaxNoteBytes < 1 {
		return nil, fmt.Errorf("message-feedback: maxNoteBytes must be a positive safe integer, got %d", cfg.MaxNoteBytes)
	}
	return &Service{
		maxNoteBytes:  cfg.MaxNoteBytes,
		opTails:       map[session.SessionID]chan struct{}{},
		admissionOpen: true,
	}, nil
}

// SetDependencies wires the persistence coordinator and live session store.
func (s *Service) SetDependencies(coordinator *persistence.Coordinator, store *session.Store) {
	s.coordinator = coordinator
	s.store = store
}

// Open initializes the sidecar domain and acquires the sessions table.
func (s *Service) Open(domain *storagedomain.Domain) error {
	s.domain = domain
	s.table = domain.Table("sessions")
	return nil
}

// Close rejects later admissions and drains in-flight operations.
func (s *Service) Close() {
	s.mu.Lock()
	s.admissionOpen = false
	tails := make([]chan struct{}, 0, len(s.opTails))
	for _, tail := range s.opTails {
		tails = append(tails, tail)
	}
	s.mu.Unlock()
	for _, tail := range tails {
		<-tail
	}
	if s.domain != nil {
		s.domain.Close()
	}
}

// List returns feedback belonging to the current persisted Session
// lifecycle. A stale row from a reused Session id is invisible.
func (s *Service) List(sessionID session.SessionID) (Result, error) {
	known, err := s.inspectSession(sessionID)
	if err != nil {
		return Result{}, err
	}
	if !known.OK {
		return known, nil
	}
	row, ok := s.currentRow(sessionID, known.Value.(persistence.Inspection).Meta)
	items := []Item{}
	if ok {
		items = row.Items
	}
	return success(items), nil
}

// Put creates or replaces feedback for one derived append-origin assistant
// message. Every request must match the addressed item's current version; a
// matching no-op returns the stored item without changing its revision.
func (s *Service) Put(sessionID session.SessionID, messageID string, rating Rating, note *string, ifVersion *Version) (Result, error) {
	resolvedNote := s.resolveNote(note)
	if !resolvedNote.OK {
		return resolvedNote, nil
	}
	var noteValue *string
	if resolvedNote.Value != nil {
		noteValue = resolvedNote.Value.(*string)
	}
	var result Result
	var opErr error
	s.serialize(sessionID, func() {
		result, opErr = s.putSerialized(sessionID, messageID, rating, noteValue, ifVersion)
	})
	if opErr != nil {
		return Result{}, opErr
	}
	return result, nil
}

// Delete removes one feedback item. Absence is successful regardless of the
// supplied version; an existing item requires an exact version match.
func (s *Service) Delete(sessionID session.SessionID, messageID string, ifVersion *Version) (Result, error) {
	var result Result
	var opErr error
	s.serialize(sessionID, func() {
		result, opErr = s.deleteSerialized(sessionID, messageID, ifVersion)
	})
	if opErr != nil {
		return Result{}, opErr
	}
	return result, nil
}

// Version is the opaque item version type alias.
type Version = string

// putSerialized runs one put inside the per-session serialization tail.
func (s *Service) putSerialized(sessionID session.SessionID, messageID string, rating Rating, note *string, ifVersion *Version) (Result, error) {
	known, err := s.inspectSession(sessionID)
	if err != nil {
		return Result{}, err
	}
	if !known.OK {
		return known, nil
	}
	inspection := known.Value.(persistence.Inspection)
	if !s.hasFeedbackTarget(inspection, messageID) {
		return rejected(&Failure{Code: FailureTargetNotFound, SessionID: &sessionID, MessageID: &messageID}), nil
	}
	durable, err := s.ensureTargetDurable(inspection)
	if err != nil {
		return Result{}, err
	}
	if !sameHeaderIdentity(durable.Meta, inspection.Meta) || !s.hasFeedbackTarget(durable, messageID) {
		return rejected(&Failure{Code: FailureTargetNotFound, SessionID: &sessionID, MessageID: &messageID}), nil
	}
	row, ok := s.currentRow(sessionID, durable.Meta)
	items := []Item{}
	if ok {
		items = row.Items
	}
	index := -1
	var existing *Item
	for i := range items {
		if items[i].MessageID == messageID {
			index = i
			existing = &items[i]
			break
		}
	}
	currentVersion := ""
	if existing != nil {
		currentVersion = existing.Version
	}
	if ifVersion == nil || *ifVersion != currentVersion {
		return rejected(s.versionConflict(existing)), nil
	}
	if existing != nil && existing.Rating == rating && sameNote(existing.Note, note) {
		return success(*existing), nil
	}
	now := time.Now().UnixMilli()
	item := Item{
		MessageID: messageID,
		Rating:    rating,
		Note:      note,
		Version:   nextVersion(),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if existing != nil {
		item.CreatedAt = existing.CreatedAt
		if now < existing.UpdatedAt {
			item.UpdatedAt = existing.UpdatedAt
		}
	}
	nextItems := append([]Item(nil), items...)
	if index == -1 {
		nextItems = append(nextItems, item)
	} else {
		nextItems[index] = item
	}
	if err := s.writeRow(sessionID, durable.Meta, nextItems); err != nil {
		return Result{}, err
	}
	return success(item), nil
}

// deleteSerialized runs one delete inside the per-session serialization tail.
func (s *Service) deleteSerialized(sessionID session.SessionID, messageID string, ifVersion *Version) (Result, error) {
	known, err := s.inspectSession(sessionID)
	if err != nil {
		return Result{}, err
	}
	if !known.OK {
		return known, nil
	}
	inspection := known.Value.(persistence.Inspection)
	row, ok := s.currentRow(sessionID, inspection.Meta)
	items := []Item{}
	if ok {
		items = row.Items
	}
	index := -1
	var existing *Item
	for i := range items {
		if items[i].MessageID == messageID {
			index = i
			existing = &items[i]
			break
		}
	}
	if existing == nil {
		return success(map[string]any{"absent": true}), nil
	}
	if ifVersion == nil || *ifVersion != existing.Version {
		return rejected(s.versionConflict(existing)), nil
	}
	nextItems := append(append([]Item(nil), items[:index]...), items[index+1:]...)
	if err := s.writeRow(sessionID, inspection.Meta, nextItems); err != nil {
		return Result{}, err
	}
	return success(map[string]any{"absent": true}), nil
}

// inspectSession uses the storage catalog as existence authority before
// inspecting the log.
func (s *Service) inspectSession(sessionID session.SessionID) (Result, error) {
	if s.coordinator == nil {
		return Result{}, fmt.Errorf("message-feedback: persistence coordinator is not wired")
	}
	live := s.store != nil && s.store.Get(sessionID) != nil
	if !live {
		snapshots, err := s.coordinator.ListSnapshots()
		if err != nil {
			return Result{}, err
		}
		found := false
		for _, snapshot := range snapshots {
			if snapshot.Header.ID == sessionID {
				found = true
				break
			}
		}
		if !found && (s.store == nil || s.store.Get(sessionID) == nil) {
			return rejected(&Failure{Code: FailureSessionNotFound, SessionID: &sessionID}), nil
		}
	}
	inspection, err := s.coordinator.Inspect(sessionID)
	if err != nil {
		return Result{}, err
	}
	return success(inspection), nil
}

// hasFeedbackTarget requires the exact finalized append-origin assistant
// message projection.
func (s *Service) hasFeedbackTarget(inspection persistence.Inspection, messageID string) bool {
	for _, event := range inspection.Events {
		if event.Type != session.EventAssistantMsg || !isAppendSurfaceEvent(event) {
			continue
		}
		message := session.DeriveEventMessage(event)
		if message != nil && message.Role == llm.RoleAssistant && string(message.ID) == messageID {
			return true
		}
	}
	return false
}

// ensureTargetDurable puts the target log prefix behind a durability barrier
// before its sidecar.
func (s *Service) ensureTargetDurable(inspection persistence.Inspection) (persistence.Inspection, error) {
	if s.store != nil {
		if live := s.store.Get(inspection.Meta.ID); live != nil && sameHeaderIdentity(live.Header(), inspection.Meta) {
			result := s.store.Flush()
			if !result.Participated {
				return persistence.Inspection{}, fmt.Errorf("message-feedback: no durability listener participated for live session %q", inspection.Meta.ID)
			}
			if result.Error != nil {
				return persistence.Inspection{}, fmt.Errorf("message-feedback: durability flush failed for live session %q: %w", inspection.Meta.ID, result.Error)
			}
		}
	}
	return s.coordinator.ReadFrom(inspection.Meta.ID, 0)
}

// resolveNote validates optional-note semantics and the configured byte bound.
func (s *Service) resolveNote(note *string) Result {
	if note == nil {
		return success(nil)
	}
	if strings.TrimSpace(*note) == "" {
		return rejected(&Failure{Code: FailureNoteBlank})
	}
	actualBytes := int64(len([]byte(*note)))
	if actualBytes > s.maxNoteBytes {
		return rejected(&Failure{Code: FailureNoteTooLarge, MaxBytes: &s.maxNoteBytes, ActualBytes: &actualBytes})
	}
	return success(note)
}

// versionConflict returns the authoritative item for a failed comparison.
func (s *Service) versionConflict(current *Item) *Failure {
	return &Failure{Code: FailureVersionConflict, Current: current}
}

// serialize queues a complete read/compare/write mutation behind this
// Session's prior mutation.
func (s *Service) serialize(sessionID session.SessionID, operation func()) {
	s.mu.Lock()
	if !s.admissionOpen {
		s.mu.Unlock()
		return
	}
	tail, ok := s.opTails[sessionID]
	if !ok {
		tail = make(chan struct{}, 1)
		tail <- struct{}{}
		s.opTails[sessionID] = tail
	}
	s.mu.Unlock()
	<-tail
	defer func() {
		tail <- struct{}{}
	}()
	operation()
}

// currentRow returns the stored row when it belongs to the inspected
// Session lifecycle.
func (s *Service) currentRow(sessionID session.SessionID, header session.SessionHeader) (Row, bool) {
	raw := s.table.Get(string(sessionID))
	if raw == nil {
		return Row{}, false
	}
	var row Row
	if err := json.Unmarshal(raw, &row); err != nil {
		return Row{}, false
	}
	if !sameIdentity(row.Session, header) {
		return Row{}, false
	}
	return row, true
}

// writeRow persists the replacement row for one lifecycle identity.
func (s *Service) writeRow(sessionID session.SessionID, header session.SessionHeader, items []Item) error {
	row := Row{Session: identityOf(header), Items: items}
	raw, err := json.Marshal(row)
	if err != nil {
		return err
	}
	return s.table.Put(string(sessionID), raw)
}

// identityOf projects the Session fields that distinguish one lifecycle.
func identityOf(header session.SessionHeader) SessionIdentity {
	identity := SessionIdentity{CreatedAt: header.CreatedAt}
	if header.CWD != "" {
		cwd := header.CWD
		identity.CWD = &cwd
	}
	return identity
}

// sameIdentity reports whether a stored row belongs to the inspected
// Session lifecycle.
func sameIdentity(row SessionIdentity, header session.SessionHeader) bool {
	if row.CreatedAt != header.CreatedAt {
		return false
	}
	return sameOptionalCWD(row.CWD, header.CWD)
}

// sameHeaderIdentity reports whether two observations name the same
// persisted Session lifecycle.
func sameHeaderIdentity(left, right session.SessionHeader) bool {
	return left.ID == right.ID && left.CreatedAt == right.CreatedAt && left.CWD == right.CWD
}

func sameOptionalCWD(left *string, right string) bool {
	if left == nil {
		return right == ""
	}
	return *left == right
}

// sameNote is the no-op gate for a matching put.
func sameNote(left, right *string) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

// isAppendSurfaceEvent reports whether the event entered the surface by
// append (a finalized assistant message candidate).
func isAppendSurfaceEvent(event session.Event) bool {
	if event.SurfaceOp == nil {
		return false
	}
	return event.SurfaceOp.Kind == session.SurfaceAppend
}

// nextVersion mints an opaque equality token for one material mutation.
func nextVersion() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Sprintf("v-%d", time.Now().UnixNano())
	}
	raw[6] = (raw[6] & 0x0f) | 0x40 // version 4
	raw[8] = (raw[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}

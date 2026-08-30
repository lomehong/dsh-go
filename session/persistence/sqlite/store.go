package sqlite

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"dshgo/session"
	"dshgo/session/persistence"
)

// BackendName is the backend's name in dispose-failure aggregates.
const BackendName = "session-persistence-sqlite"

// sessionRow is one materialized session's metadata and monotonic revision.
type sessionRow struct {
	id              int64
	sessionKey      string
	version         int64
	createdAt       int64
	cwd             sql.NullString
	parentSession   sql.NullString
	seedLength      sql.NullInt64
	origin          sql.NullString
	delegationDepth sql.NullInt64
	agentPreset     sql.NullString
	incarnation     string
	revision        int64
}

// eventRow is one physical event row; packed rows represent multiple logical
// events and are refused loud by this build.
type eventRow struct {
	seq             int64
	eventType       string
	time            int64
	data            []byte
	sourceEventSeqs []byte
	surfaceOp       sql.NullString
	isPacked        int64
}

// AppendBatch durably appends one contiguous batch, creating the header row
// when the session is not yet materialized.
func (s *Store) AppendBatch(meta session.SessionHeader, events []session.Event, materialized bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.openDB(); err != nil {
		return err
	}
	if len(events) == 0 {
		return nil
	}
	return s.immediate(func() error {
		sessionKey, err := s.materializedKey(meta, materialized)
		if err != nil {
			return err
		}
		rows, err := s.eventRows(sessionKey)
		if err != nil {
			return err
		}
		lastSeq, err := lastPhysicalSeq(meta.ID, rows)
		if err != nil {
			return err
		}
		if events[0].Seq != lastSeq+1 {
			return fmt.Errorf("session %s append starts at seq %d, stored next seq is %d", meta.ID, events[0].Seq, lastSeq+1)
		}
		for _, event := range events {
			if err := s.insertEvent(sessionKey, event, false); err != nil {
				return err
			}
		}
		return s.incrementRevision(meta.ID)
	})
}

// LoadStored reads a stored prefix by id: the header row, its full event
// span, the source-qualified revision, and the torn-tail marker when the
// physical tail breaks seq contiguity.
func (s *Store) LoadStored(id session.SessionID) (*persistence.StoredPrefix, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.openDB(); err != nil {
		return nil, err
	}
	var prefix *persistence.StoredPrefix
	err := s.begin(func() error {
		row, err := s.rowFor(id)
		if err != nil {
			return err
		}
		if row == nil {
			return nil
		}
		rows, err := s.eventRows(row.id)
		if err != nil {
			return err
		}
		events, tornFrom, err := scanRows(rows, 0)
		if err != nil {
			return err
		}
		prefix = &persistence.StoredPrefix{
			Meta:     rowToMeta(row),
			Events:   events,
			Revision: sqliteRevision(s.storeID, row),
		}
		if tornFrom != nil {
			prefix.TornMarker = *tornFrom
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return prefix, nil
}

// ReadStoredRevision reads the current source-qualified revision without
// loading the event log. An absent identity yields an empty revision.
func (s *Store) ReadStoredRevision(id session.SessionID) (persistence.Revision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.openDB(); err != nil {
		return "", err
	}
	var revision persistence.Revision
	err := s.begin(func() error {
		row, err := s.rowFor(id)
		if err != nil {
			return err
		}
		if row != nil {
			revision = sqliteRevision(s.storeID, row)
		}
		return nil
	})
	return revision, err
}

// ReadStoredFrom returns the header plus stored events with seq >= fromSeq
// (the SuffixReader hook; SQL addresses rows by seq directly).
func (s *Store) ReadStoredFrom(id session.SessionID, fromSeq int64) (*persistence.StoredSuffix, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.openDB(); err != nil {
		return nil, err
	}
	var suffix *persistence.StoredSuffix
	err := s.begin(func() error {
		row, err := s.rowFor(id)
		if err != nil {
			return err
		}
		if row == nil {
			return nil
		}
		rows, err := s.eventRowsFrom(row.id, fromSeq)
		if err != nil {
			return err
		}
		events, tornFrom, err := scanRows(rows, fromSeq)
		if err != nil {
			return err
		}
		if tornFrom != nil {
			return fmt.Errorf("session %s has an invalid physical tail at seq %d", id, *tornFrom)
		}
		suffix = &persistence.StoredSuffix{Meta: rowToMeta(row), Events: events}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return suffix, nil
}

// CommitRepair makes a crash repair durable: truncate the torn tail and
// append closers, refusing stale repairs. One transaction covers both steps
// (the file backend's two-durable-step allowance does not apply here).
func (s *Store) CommitRepair(meta session.SessionHeader, tornMarker any, closers []session.Event) error {
	if tornMarker == nil && len(closers) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.openDB(); err != nil {
		return err
	}
	return s.immediate(func() error {
		row, err := s.rowFor(meta.ID)
		if err != nil {
			return err
		}
		if row == nil {
			return fmt.Errorf("session %s metadata row is missing", meta.ID)
		}
		rows, err := s.eventRows(row.id)
		if err != nil {
			return err
		}
		current, currentTorn, err := scanRows(rows, 0)
		if err != nil {
			return err
		}
		if tornMarker != nil {
			marker, ok := tornMarker.(int64)
			if !ok {
				return fmt.Errorf("session %s repair carries a foreign torn marker %v", meta.ID, tornMarker)
			}
			if currentTorn == nil || *currentTorn != marker {
				return fmt.Errorf("session %s repair is stale: physical tail no longer starts at seq %d", meta.ID, marker)
			}
			if _, err := s.exec("DELETE FROM events WHERE session_id = ? AND seq >= ?", row.id, marker); err != nil {
				return fmt.Errorf("sqlite: truncate torn tail: %w", err)
			}
		} else if currentTorn != nil {
			return fmt.Errorf("session %s repair omitted current torn tail at seq %d", meta.ID, *currentTorn)
		}
		if len(closers) > 0 {
			// Next append position, from the preserved prefix (the torn tail
			// is excluded — truncation above removed it).
			expected := int64(0)
			if len(current) > 0 {
				expected = current[len(current)-1].Seq + 1
			}
			if closers[0].Seq != expected {
				return fmt.Errorf("session %s repair is stale: closer starts at seq %d, stored next seq is %d", meta.ID, closers[0].Seq, expected)
			}
			for _, closer := range closers {
				if err := s.insertEvent(row.id, closer, false); err != nil {
					return err
				}
			}
		}
		return s.incrementRevision(meta.ID)
	})
}

// List enumerates materialized sessions, one header per session.
func (s *Store) List() ([]session.SessionHeader, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.openDB(); err != nil {
		return nil, err
	}
	rows, err := s.sessionRows()
	if err != nil {
		return nil, err
	}
	headers := make([]session.SessionHeader, 0, len(rows))
	for _, row := range rows {
		headers = append(headers, rowToMeta(row))
	}
	return headers, nil
}

// ListSnapshots lists materialized sessions with cheap per-log change tokens.
func (s *Store) ListSnapshots() ([]persistence.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.openDB(); err != nil {
		return nil, err
	}
	rows, err := s.sessionRows()
	if err != nil {
		return nil, err
	}
	snapshots := make([]persistence.Snapshot, 0, len(rows))
	for _, row := range rows {
		snapshots = append(snapshots, persistence.Snapshot{
			Header:   rowToMeta(row),
			Revision: sqliteRevision(s.storeID, row),
		})
	}
	return snapshots, nil
}

// MaterializeHeader durably creates a header-only session artifact
// (the HeaderMaterializer hook behind EnsureMaterialized).
func (s *Store) MaterializeHeader(meta session.SessionHeader) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.openDB(); err != nil {
		return err
	}
	return s.immediate(func() error {
		_, err := s.upsertSession(meta)
		return err
	})
}

// Name is the human-readable backend name.
func (s *Store) Name() string { return BackendName }

// Close releases the database handle after the coordinator reaches quiescence.
// Idempotent; later operations fail loud instead of reopening.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if !s.opened {
		return nil
	}
	s.opened = false
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("sqlite: close: %w", err)
	}
	s.db = nil
	return nil
}

// rowToMeta renders the header from its physical row.
func rowToMeta(row *sessionRow) session.SessionHeader {
	header := session.SessionHeader{
		Version:   row.version,
		ID:        session.SessionID(row.sessionKey),
		CreatedAt: row.createdAt,
	}
	if row.cwd.Valid {
		header.CWD = row.cwd.String
	}
	if row.parentSession.Valid {
		header.ParentSession = session.SessionID(row.parentSession.String)
	}
	if row.seedLength.Valid {
		seed := row.seedLength.Int64
		header.SeedLength = &seed
	}
	if row.origin.Valid {
		header.Origin = row.origin.String
	}
	if row.delegationDepth.Valid {
		depth := row.delegationDepth.Int64
		header.DelegationDepth = &depth
	}
	if row.agentPreset.Valid {
		header.AgentPreset = row.agentPreset.String
	}
	return header
}

// sqliteRevision composes the source-qualified revision: store identity,
// per-session incarnation (stamped at upsert), and the monotonic revision
// counter.
func sqliteRevision(storeIdentity string, row *sessionRow) persistence.Revision {
	return persistence.Revision(fmt.Sprintf("%s:incarnation:%s:revision:%d", storeIdentity, row.incarnation, row.revision))
}

// scanRows ports the official scanRows: rows decode in seq order from the
// base; a row that breaks contiguity or fails to decode becomes the torn
// tail (tornFrom = its physical seq) unless it sits at or before the last
// committed turn/end row, where it is hard corruption. The official packed
// rows decode to multiple logical events; this build writes one event per
// unpacked row.
func scanRows(rows []eventRow, base int64) ([]session.Event, *int64, error) {
	lastTurnEndRow := -1
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].eventType == "turn/end" && rows[i].isPacked == 0 {
			lastTurnEndRow = i
			break
		}
	}

	preserved := make([]session.Event, 0, len(rows))
	expected := base
	for i, row := range rows {
		event, err := rowToEvent(row)
		if err != nil {
			if i <= lastTurnEndRow {
				return nil, nil, fmt.Errorf("corrupt session log: invalid committed physical row at seq %d", row.seq)
			}
			torn := row.seq
			return preserved, &torn, nil
		}
		if event.Seq != expected {
			if i <= lastTurnEndRow {
				return nil, nil, fmt.Errorf("corrupt session log: invalid committed physical row at seq %d", row.seq)
			}
			torn := row.seq
			return preserved, &torn, nil
		}
		expected++
		preserved = append(preserved, event)
	}
	return preserved, nil, nil
}

// lastPhysicalSeq reports the highest contiguous physical seq of the stored
// span and refuses a span broken inside its committed prefix (an append
// would otherwise build on corrupt state).
func lastPhysicalSeq(id session.SessionID, rows []eventRow) (int64, error) {
	if len(rows) == 0 {
		return -1, nil
	}
	_, torn, err := scanRows(rows, rows[0].seq)
	if err != nil {
		return 0, fmt.Errorf("session %s: %w", id, err)
	}
	if torn != nil {
		return 0, fmt.Errorf("session %s has an invalid physical tail at seq %d", id, *torn)
	}
	return rows[len(rows)-1].seq, nil
}

// rowToEvent decodes one physical row; packed rows are refused loud (this
// build writes none — see the honest degradations in the package comment).
func rowToEvent(row eventRow) (session.Event, error) {
	if row.isPacked != 0 {
		return session.Event{}, fmt.Errorf("sqlite: packed event row at seq %d is not readable by this build", row.seq)
	}
	event := session.Event{
		Type: row.eventType,
		Seq:  row.seq,
		Time: row.time,
		Data: append(json.RawMessage(nil), row.data...),
	}
	if len(row.sourceEventSeqs) > 0 {
		if err := json.Unmarshal(row.sourceEventSeqs, &event.SourceEventSeqs); err != nil {
			return session.Event{}, fmt.Errorf("sqlite: source_event_seqs decode at seq %d: %w", row.seq, err)
		}
	}
	if row.surfaceOp.Valid && row.surfaceOp.String != "" {
		op := &session.SurfaceOp{}
		if err := json.Unmarshal([]byte(row.surfaceOp.String), op); err != nil {
			return session.Event{}, fmt.Errorf("sqlite: surface_op decode at seq %d: %w", row.seq, err)
		}
		event.SurfaceOp = op
	}
	return event, nil
}

// insertEvent binds one logical event into its physical row. sourceEventSeqs
// preserves presence: a present empty array stays a JSON `[]`, nil stays
// NULL.
func (s *Store) insertEvent(sessionKey int64, event session.Event, packed bool) error {
	var sourceEventSeqs any
	if event.SourceEventSeqs != nil {
		encoded, err := json.Marshal(event.SourceEventSeqs)
		if err != nil {
			return fmt.Errorf("sqlite: source_event_seqs encode at seq %d: %w", event.Seq, err)
		}
		sourceEventSeqs = string(encoded)
	}
	var surfaceOp any
	if event.SurfaceOp != nil {
		encoded, err := json.Marshal(event.SurfaceOp)
		if err != nil {
			return fmt.Errorf("sqlite: surface_op encode at seq %d: %w", event.Seq, err)
		}
		surfaceOp = string(encoded)
	}
	isPacked := int64(0)
	if packed {
		isPacked = 1
	}
	if _, err := s.exec(
		"INSERT INTO events (session_id, seq, type, time, data, source_event_seqs, surface_op, is_packed) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		sessionKey, event.Seq, event.Type, event.Time, string(event.Data), sourceEventSeqs, surfaceOp, isPacked,
	); err != nil {
		return fmt.Errorf("sqlite: insert event seq %d: %w", event.Seq, err)
	}
	return nil
}

// materializedKey resolves the integer session key, upserting the header row
// (with a fresh incarnation UUID) when the session is not yet materialized.
func (s *Store) materializedKey(meta session.SessionHeader, materialized bool) (int64, error) {
	if materialized {
		return s.keyFor(meta.ID)
	}
	key, err := s.upsertSession(meta)
	if err != nil {
		return 0, err
	}
	return key, nil
}

// upsertSession inserts or refreshes the header row, stamping a new
// incarnation on every write (the official upsert's UUID binding).
func (s *Store) upsertSession(meta session.SessionHeader) (int64, error) {
	var id int64
	err := s.db.QueryRow(
		`INSERT INTO sessions (session_key, version, created_at, cwd, parent_session, seed_length, origin, delegation_depth, agent_preset, incarnation, revision)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)
		 ON CONFLICT(session_key) DO UPDATE SET
		   version = excluded.version, created_at = excluded.created_at, cwd = excluded.cwd,
		   parent_session = excluded.parent_session, seed_length = excluded.seed_length,
		   origin = excluded.origin, delegation_depth = excluded.delegation_depth,
		   agent_preset = excluded.agent_preset, incarnation = excluded.incarnation
		 RETURNING id`,
		string(meta.ID), meta.Version, meta.CreatedAt, nullableString(meta.CWD), nullableString(string(meta.ParentSession)),
		nullableInt(meta.SeedLength), nullableString(meta.Origin), nullableInt(meta.DelegationDepth),
		nullableString(meta.AgentPreset), newUUID(),
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("sqlite: upsert session %s: %w", meta.ID, err)
	}
	return id, nil
}

func (s *Store) keyFor(id session.SessionID) (int64, error) {
	var key int64
	err := s.db.QueryRow("SELECT id FROM sessions WHERE session_key = ?", string(id)).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("session %s metadata row is missing", id)
	}
	if err != nil {
		return 0, fmt.Errorf("sqlite: session key read: %w", err)
	}
	return key, nil
}

func (s *Store) rowFor(id session.SessionID) (*sessionRow, error) {
	row := &sessionRow{}
	err := s.db.QueryRow(
		`SELECT id, session_key, version, created_at, cwd, parent_session, seed_length, origin, delegation_depth, agent_preset, incarnation, revision
		 FROM sessions WHERE session_key = ?`, string(id),
	).Scan(&row.id, &row.sessionKey, &row.version, &row.createdAt, &row.cwd, &row.parentSession, &row.seedLength,
		&row.origin, &row.delegationDepth, &row.agentPreset, &row.incarnation, &row.revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: session read: %w", err)
	}
	return row, nil
}

func (s *Store) eventRows(sessionKey int64) ([]eventRow, error) {
	return s.queryEventRows(
		"SELECT seq, type, time, data, source_event_seqs, surface_op, is_packed FROM events WHERE session_id = ? ORDER BY seq", sessionKey)
}

func (s *Store) eventRowsFrom(sessionKey int64, fromSeq int64) ([]eventRow, error) {
	return s.queryEventRows(
		"SELECT seq, type, time, data, source_event_seqs, surface_op, is_packed FROM events WHERE session_id = ? AND seq >= ? ORDER BY seq", sessionKey, fromSeq)
}

func (s *Store) queryEventRows(query string, args ...any) ([]eventRow, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: event query: %w", err)
	}
	defer rows.Close()
	var out []eventRow
	for rows.Next() {
		var row eventRow
		if err := rows.Scan(&row.seq, &row.eventType, &row.time, &row.data, &row.sourceEventSeqs, &row.surfaceOp, &row.isPacked); err != nil {
			return nil, fmt.Errorf("sqlite: event row scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: event rows: %w", err)
	}
	return out, nil
}

func (s *Store) sessionRows() ([]*sessionRow, error) {
	rows, err := s.db.Query(
		`SELECT id, session_key, version, created_at, cwd, parent_session, seed_length, origin, delegation_depth, agent_preset, incarnation, revision
		 FROM sessions ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: sessions query: %w", err)
	}
	defer rows.Close()
	var out []*sessionRow
	for rows.Next() {
		row := &sessionRow{}
		if err := rows.Scan(&row.id, &row.sessionKey, &row.version, &row.createdAt, &row.cwd, &row.parentSession, &row.seedLength,
			&row.origin, &row.delegationDepth, &row.agentPreset, &row.incarnation, &row.revision); err != nil {
			return nil, fmt.Errorf("sqlite: session row scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// incrementRevision bumps the session's monotonic revision counter; the row
// must exist.
func (s *Store) incrementRevision(id session.SessionID) error {
	result, err := s.exec("UPDATE sessions SET revision = revision + 1 WHERE session_key = ?", string(id))
	if err != nil {
		return fmt.Errorf("sqlite: revision update: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return fmt.Errorf("session %s metadata row is missing", id)
	}
	return nil
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableInt(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func newUUID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		panic("sqlite: uuid source unavailable: " + err.Error())
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16])
}

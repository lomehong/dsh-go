// Package jsonl is the JSONL session-persistence backend: path-safe
// directory layout, the header-line format, the incremental log scanner with
// torn-tail truncation, and the file operations over them. Port of
// packages/session/session-persistence-jsonl/src/format.ts plus the file
// backend it underpins.
package jsonl

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"

	"dshgo/session"
	"dshgo/session/persistence"
)

// Compression selects the physical artifact encoding: plaintext lines, or
// the concatenated-Zstandard-frame container (one independently decodable,
// checksummed frame per durable batch).
type Compression string

// Supported physical encodings.
const (
	CompressionNone Compression = "none"
	CompressionZstd Compression = "zstd"
)

// LogSuffix returns the artifact suffix for one physical encoding.
func LogSuffix(compression Compression) string {
	if compression == "zstd" {
		return ".jsonl.zstd"
	}
	return ".jsonl"
}

// SessionFormatUnsupportedError reports a log whose format version this
// build does not read. The user must see "upgrade the harness", never
// "corrupt session log".
type SessionFormatUnsupportedError struct {
	ID      string
	Version int64
}

func (e *SessionFormatUnsupportedError) Error() string {
	// The shared direction-aware refusal text: a newer log says "upgrade
	// the harness", an older one says the era is no longer read.
	return persistence.SessionFormatVersionRefusal(e.ID, e.Version)
}

// EncodeSegment encodes an arbitrary string as a single safe path segment,
// injectively over ALL strings. A SessionID is an unvalidated string, so
// this neutralizes ../, absolute paths, NUL, and separators before any
// filesystem use. Safe code units remain literal; every other unit,
// including ~, becomes ~XXXX. Operating on UTF-16 code units matches the
// official encoder, while special-casing "." and ".." prevents traversal by
// an otherwise safe whole segment.
func EncodeSegment(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("cannot encode an empty path segment")
	}
	if raw == "." {
		return "~002E", nil
	}
	if raw == ".." {
		return "~002E~002E", nil
	}
	var out strings.Builder
	for _, unit := range utf16Units(raw) {
		if isSafeUnit(unit) {
			out.WriteRune(rune(unit))
		} else {
			fmt.Fprintf(&out, "~%04X", unit)
		}
	}
	return out.String(), nil
}

// utf16Units decodes a Go string into UTF-16 code units, the official
// encoder's unit space (surrogate pairs for non-BMP runes).
func utf16Units(raw string) []uint16 {
	units := utf16.Encode([]rune(raw))
	return units
}

func isSafeUnit(unit uint16) bool {
	if unit == '~' {
		return false
	}
	c := rune(unit)
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-'
}

// ProjectKey builds the readable directory key for a project path:
// filesystem and drive separators become `-`; unsafe code units use the same
// ~XXXX escape as session ids; the key is bounded for filesystem component
// limits. Separator replacement and truncation are intentionally lossy,
// following the common human-navigable project-directory convention.
func ProjectKey(cwd string) string {
	if cwd == "" {
		panic("cannot encode an empty project path")
	}
	var readable strings.Builder
	separatorRun := false
	for _, unit := range utf16Units(cwd) {
		c := rune(unit)
		switch {
		case c == '/' || c == '\\' || c == ':':
			if !separatorRun {
				readable.WriteRune('-')
			}
			separatorRun = true
		case isSafeUnit(unit):
			readable.WriteRune(c)
			separatorRun = false
		default:
			fmt.Fprintf(&readable, "~%04X", unit)
			separatorRun = false
		}
	}
	slug := strings.TrimLeft(readable.String(), "-")
	if slug == "" {
		slug = "root"
	}
	if len(slug) > 251 {
		slug = slug[:251]
	}
	return "--" + slug + "--"
}

// ProjectDir is the configured root's human-navigable project directory; an
// empty cwd selects _no-cwd.
func ProjectDir(root, cwd string) string {
	if cwd == "" {
		return filepath.Join(root, "_no-cwd")
	}
	return filepath.Join(root, ProjectKey(cwd))
}

// SessionDir is the directory owned by one session, available for future
// session-local artifacts.
func SessionDir(root, cwd, id string) string {
	encoded, err := EncodeSegment(id)
	if err != nil {
		panic(fmt.Sprintf("session id %q: %v", id, err))
	}
	return filepath.Join(ProjectDir(root, cwd), encoded)
}

// LogPath is the append-only event-log file path for a session at the
// CURRENT format generation: v0 keeps the original `session.jsonl`; every
// later generation carries `.vN` before the suffix (official
// sessionFormatLogFilename).
func LogPath(root, cwd, id string, compression Compression) string {
	return filepath.Join(SessionDir(root, cwd, id), GenerationBasename(session.SESSION_FORMAT_VERSION)+LogSuffix(compression))
}

// GenerationBasename is the canonical raw basename of one generation,
// WITHOUT the LogSuffix (v0 → "session", v2 → "session.v2").
func GenerationBasename(version int64) string {
	if version <= 0 {
		return "session"
	}
	return "session.v" + fmt.Sprintf("%d", version)
}

// SelectGenerationLog picks the highest canonical generation log from one
// session directory (official list policy: numerically highest wins;
// predecessors provide no fallback). ok=false when none exists.
func SelectGenerationLog(dir string, compression Compression) (string, int64, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", 0, false
	}
	best := ""
	var bestVersion int64 = -1
	suffix := LogSuffix(compression)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, suffix) {
			continue
		}
		version, ok := parseGenerationFilename(strings.TrimSuffix(name, suffix))
		if !ok {
			continue
		}
		if version > bestVersion {
			bestVersion = version
			best = filepath.Join(dir, name)
		}
	}
	if bestVersion < 0 {
		return "", 0, false
	}
	return best, bestVersion, true
}

// parseGenerationFilename parses the raw basename (LogSuffix stripped):
// `session` is generation 0; `session.vN` generation N (uppercase,
// leading-zero, and v0 forms are not canonical).
func parseGenerationFilename(raw string) (int64, bool) {
	if raw == "session" {
		return 0, true
	}
	digits := strings.TrimPrefix(raw, "session.v")
	if digits == raw || digits == "" {
		return 0, false
	}
	if digits[0] == '0' {
		return 0, false
	}
	var version int64
	for i := 0; i < len(digits); i++ {
		c := digits[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		version = version*10 + int64(c-'0')
	}
	if version <= 0 {
		return 0, false
	}
	return version, true
}

// headerLine is the first JSONL record of a session artifact: the immutable
// session header tagged as a `session` record. delegationDepth is always
// serialized (0 default); absent optional fields are omitted, never null.
type headerLine struct {
	Type          string  `json:"type"`
	Version       int64   `json:"version"`
	ID            string  `json:"id"`
	CreatedAt     int64   `json:"createdAt"`
	CWD           *string `json:"cwd,omitempty"`
	ParentSession *string `json:"parentSession,omitempty"`
	// v0/v1 physical headers carry the numeric seedLength; the v2 physical
	// header carries the boolean isSeeded and no cut (the cut derives from
	// the last inherited end-seed marker at body-read time).
	SeedLength      *int64  `json:"seedLength,omitempty"`
	IsSeeded        *bool   `json:"isSeeded,omitempty"`
	Origin          *string `json:"origin,omitempty"`
	DelegationDepth int64   `json:"delegationDepth"`
	AgentPreset     *string `json:"agentPreset,omitempty"`
}

func toHeaderLine(header session.SessionHeader) headerLine {
	line := headerLine{
		Type:      "session",
		Version:   header.Version,
		ID:        string(header.ID),
		CreatedAt: header.CreatedAt,
	}
	if header.CWD != "" {
		line.CWD = &header.CWD
	}
	if header.ParentSession != "" {
		parent := string(header.ParentSession)
		line.ParentSession = &parent
	}
	// v0/v1 physical headers keep the numeric seedLength (seeded →
	// inheritedEventCount); v2 physical headers carry only the boolean
	// isSeeded and no cut (official toHeaderLine per generation).
	if header.Version >= 2 {
		if header.IsSeeded {
			seeded := true
			line.IsSeeded = &seeded
		}
	} else if header.IsSeeded {
		seed := int64(header.InheritedEventCount)
		line.SeedLength = &seed
	}
	if header.Origin != "" {
		line.Origin = &header.Origin
	}
	if header.DelegationDepth != nil {
		line.DelegationDepth = *header.DelegationDepth
	}
	if header.AgentPreset != "" {
		line.AgentPreset = &header.AgentPreset
	}
	return line
}

func headerFromLine(line headerLine) session.SessionHeader {
	header := session.SessionHeader{
		Version:   line.Version,
		ID:        session.SessionID(line.ID),
		CreatedAt: line.CreatedAt,
	}
	if line.CWD != nil {
		header.CWD = *line.CWD
	}
	if line.ParentSession != nil {
		header.ParentSession = session.SessionID(*line.ParentSession)
	}
	if line.SeedLength != nil {
		header.IsSeeded = true
		header.InheritedEventCount = session.SessionLogOffset(*line.SeedLength)
	} else if line.IsSeeded != nil && *line.IsSeeded {
		// v2: the header states seeded but stores no cut; the body scan
		// derives the exact cut from the last inherited end-seed marker.
		header.IsSeeded = true
	}
	if line.Origin != nil {
		header.Origin = *line.Origin
	}
	depth := line.DelegationDepth
	header.DelegationDepth = &depth
	if line.AgentPreset != nil {
		header.AgentPreset = *line.AgentPreset
	}
	return header
}

// decodeHeaderLine parses one header line's JSON value with the official
// guards: retired policy baseline fields refuse, foreign format versions
// refuse as SessionFormatUnsupportedError BEFORE any structural check.
func decodeHeaderLine(parsed map[string]any) (session.SessionHeader, error) {
	for _, retired := range []string{"sandboxMode", "approvalPolicy"} {
		if _, ok := parsed[retired]; ok {
			return session.SessionHeader{}, errors.New("session header uses retired policy baseline fields")
		}
	}
	if versionRaw, ok := parsed["version"]; !ok {
		return session.SessionHeader{}, errors.New("corrupt session log: first line is not a session header")
	} else if version, ok := versionRaw.(json.Number); ok {
		value, err := version.Int64()
		if err == nil && value > session.SESSION_FORMAT_VERSION {
			id := "<unknown>"
			if raw, ok := parsed["id"].(string); ok {
				id = raw
			}
			return session.SessionHeader{}, &SessionFormatUnsupportedError{ID: id, Version: value}
		}
		// Historical generations and the v2 header shape are mutually
		// exclusive: a v2 physical header never carries a numeric cut.
		if err == nil && value >= 2 {
			if _, hasSeedLength := parsed["seedLength"]; hasSeedLength {
				return session.SessionHeader{}, errors.New("corrupt session log: format v2 header must not carry a numeric seed cut")
			}
		}
	}
	if _, ok := parsed["delegationDepth"]; !ok {
		return session.SessionHeader{}, errors.New("corrupt session log: first line is not a session header")
	}
	raw, err := json.Marshal(parsed)
	if err != nil {
		return session.SessionHeader{}, err
	}
	var line headerLine
	if err := json.Unmarshal(raw, &line); err != nil {
		return session.SessionHeader{}, err
	}
	// Header-only reads translate historical generations in memory (they
	// never publish a successor); only a NEWER generation refused above, and
	// a non-session record is not a header at all.
	if line.Type != "session" || line.Version < 0 || line.Version > session.SESSION_FORMAT_VERSION ||
		line.ID == "" || line.CreatedAt < 0 || line.Origin != nil && *line.Origin != "subagent" {
		return session.SessionHeader{}, errors.New("corrupt session log: first line is not a session header")
	}
	return headerFromLine(line), nil
}

// parseHeaderRecord parses one complete newline-terminated header record.
func parseHeaderRecord(record []byte) (session.SessionHeader, error) {
	if len(record) == 0 || record[len(record)-1] != 0x0A || bytes.IndexByte(record, 0x0A) != len(record)-1 {
		return session.SessionHeader{}, errors.New("empty or header-less session log")
	}
	parsed, err := parseLineValue(record[:len(record)-1])
	if err != nil {
		return session.SessionHeader{}, errors.New("corrupt session log: header line is not valid JSON")
	}
	return decodeHeaderLine(parsed)
}

// parseLineValue parses one JSONL line, preserving integer exactness via
// json.Number.
func parseLineValue(line []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.UseNumber()
	var parsed map[string]any
	if err := decoder.Decode(&parsed); err != nil {
		return nil, err
	}
	return parsed, nil
}

// LogScan is a complete or torn log's preserved state.
type LogScan struct {
	Meta session.SessionHeader
	// Events is the contiguous preserved prefix.
	Events []session.Event
	// CommittedBytes is the byte offset safe to append at (end of the last
	// complete, in-sequence event line).
	CommittedBytes int64
	// TornTail reports unreadable bytes beyond the committed prefix. The
	// scan never mutates the artifact: truncation is the caller's repair
	// decision (the coordinator commits it through CommitRepair).
	TornTail bool
}

// SessionLogScanner incrementally scans complete JSONL event records after
// an independently supplied header record. Newline search and byte offsets
// stay on raw bytes; only complete records are decoded. A corruption issue
// inside the committed region truncates the preserved prefix, unless a
// later complete line is a turn/end — a turn boundary after corruption
// cannot be explained by a torn final write, so the log fails loud.
type SessionLogScanner struct {
	meta           session.SessionHeader
	events         []session.Event
	fragment       []byte
	inputBytes     int64
	committedBytes int64
	eventLine      int
	issue          error
	finished       bool
}

// NewSessionLogScanner creates an event scanner from exactly one
// newline-terminated header record.
func NewSessionLogScanner(headerRecord []byte) (*SessionLogScanner, error) {
	meta, err := parseHeaderRecord(headerRecord)
	if err != nil {
		return nil, err
	}
	return &SessionLogScanner{
		meta:           meta,
		inputBytes:     int64(len(headerRecord)),
		committedBytes: int64(len(headerRecord)),
	}, nil
}

// Write consumes the next raw plaintext chunk, retaining only an incomplete
// final record. A corruption issue proven fatal by a later turn/end line
// aborts the scan and is returned.
func (s *SessionLogScanner) Write(chunk []byte) error {
	if s.finished {
		return errors.New("cannot write to a finished session log scanner")
	}
	chunkStart := s.inputBytes
	s.inputBytes += int64(len(chunk))
	lineStart := 0
	for {
		newline := bytes.IndexByte(chunk[lineStart:], 0x0A)
		if newline == -1 {
			break
		}
		newline += lineStart
		line := chunk[lineStart:newline]
		if len(s.fragment) > 0 {
			line = append(append([]byte{}, s.fragment...), line...)
			s.fragment = nil
		}
		if err := s.consumeEventLine(line, chunkStart+int64(newline)+1); err != nil {
			return err
		}
		lineStart = newline + 1
	}
	if lineStart < len(chunk) {
		s.fragment = append(s.fragment, chunk[lineStart:]...)
	}
	return nil
}

func (s *SessionLogScanner) consumeEventLine(line []byte, endByte int64) error {
	s.eventLine++
	decoded, err := s.decodeEventLine(line)
	if err != nil {
		if s.issue == nil {
			s.issue = fmt.Errorf("corrupt session log: unparsable committed event at line %d", s.eventLine)
		}
		return nil
	}
	if s.issue != nil {
		for _, event := range decoded {
			if event.Type == session.EventTurnEnd {
				return s.issue
			}
		}
		return nil
	}
	rowStart := len(s.events)
	for _, event := range decoded {
		if event.Seq != int64(len(s.events)) {
			expected := len(s.events)
			s.events = s.events[:rowStart]
			s.issue = fmt.Errorf(
				"corrupt session log: seq gap in committed region at line %d (expected %d, got %d)",
				s.eventLine, expected, event.Seq)
			for _, candidate := range decoded {
				if candidate.Type == session.EventTurnEnd {
					return s.issue
				}
			}
			return nil
		}
		s.events = append(s.events, event)
	}
	s.committedBytes = endByte
	return nil
}

func (s *SessionLogScanner) decodeEventLine(line []byte) ([]session.Event, error) {
	parsed, err := parseLineValue(line)
	if err != nil {
		return nil, err
	}
	// Storage-form provenance expands before record decoding; provenance is
	// validated against the record's own seq.
	if raw, ok := parsed["sourceEventSeqs"]; ok {
		seqNumber, ok := parsed["seq"].(json.Number)
		if !ok {
			return nil, errors.New("stored session event seq must be a non-negative safe integer")
		}
		seq, err := seqNumber.Int64()
		if err != nil || seq < 0 {
			return nil, errors.New("stored session event seq must be a non-negative safe integer")
		}
		expanded, err := session.DecodeSeqRanges(raw, seq)
		if err != nil {
			return nil, err
		}
		parsed["sourceEventSeqs"] = expanded
	}
	return session.DecodeStorageRecord(parsed)
}

// Finish ends scanning, ignoring a final record without a newline as a torn
// tail. A non-fatal corruption issue still yields the preserved prefix: the
// caller truncates to CommittedBytes and the interrupted-turn repair closes
// the log. A corruption proven fatal by a later complete turn/end line
// already aborted the scan through Write.
func (s *SessionLogScanner) Finish() (LogScan, error) {
	s.finished = true
	// Format v2 stores no numeric cut: a seeded artifact marks its exact
	// cut with the last inherited end-seed marker (official
	// deriveInheritedEventCount). Seeded without a marker, or unseeded with
	// one, refuses.
	if s.meta.Version >= 2 {
		cut, hasCut, err := deriveInheritedCut(s.events)
		if err != nil {
			return LogScan{}, err
		}
		if s.meta.IsSeeded {
			if !hasCut {
				return LogScan{}, errors.New("corrupt session log: seeded format v2 Session lacks an inherited end-seed marker")
			}
			s.meta.InheritedEventCount = session.SessionLogOffset(cut)
		} else if hasCut {
			return LogScan{}, errors.New("corrupt session log: unseeded format v2 Session contains an inherited end-seed marker")
		}
	}
	return LogScan{
		Meta:           s.meta,
		Events:         s.events,
		CommittedBytes: s.committedBytes,
		TornTail:       s.committedBytes < s.inputBytes,
	}, nil
}

// deriveInheritedCut reports the cut named by the last inherited marker.
func deriveInheritedCut(events []session.Event) (int64, bool, error) {
	cut := int64(0)
	has := false
	for _, event := range events {
		if event.Type != session.EventEndSeed {
			continue
		}
		parsed, err := parseLineValue(event.Data)
		if err != nil {
			return 0, false, fmt.Errorf("corrupt session log: %s %d data is not valid JSON", event.Type, event.Seq)
		}
		if parsed["inherited"] == true {
			cut = event.Seq
			has = true
		}
	}
	return cut, has, nil
}

// ScanLog parses a complete or torn JSONL buffer into its preserved event
// prefix.
func ScanLog(buffer []byte) (LogScan, error) {
	headerEnd := bytes.IndexByte(buffer, 0x0A)
	if headerEnd == -1 {
		return LogScan{}, errors.New("empty or header-less session log")
	}
	scanner, err := NewSessionLogScanner(buffer[:headerEnd+1])
	if err != nil {
		return LogScan{}, err
	}
	if err := scanner.Write(buffer[headerEnd+1:]); err != nil {
		return LogScan{}, err
	}
	return scanner.Finish()
}

// ParseHeaderMeta parses just the header line of a log, or reports false if
// it is missing or not a header. List() reads session metadata WITHOUT
// parsing the whole log: a session picker scales with the number of
// sessions, not the total size of every conversation.
func ParseHeaderMeta(firstLine string) (session.SessionHeader, bool, error) {
	parsed, err := parseLineValue([]byte(firstLine))
	if err != nil {
		return session.SessionHeader{}, false, nil
	}
	header, err := decodeHeaderLine(parsed)
	if err != nil {
		var unsupported *SessionFormatUnsupportedError
		if errors.As(err, &unsupported) {
			return session.SessionHeader{}, false, err
		}
		return session.SessionHeader{}, false, nil
	}
	return header, true, nil
}

// Store is the JSONL file backend over one session root directory.
type Store struct {
	Root        string
	Compression Compression
}

func (st *Store) suffix() Compression {
	if st.Compression == "" {
		return CompressionNone
	}
	return st.Compression
}

// encodeArtifact wraps plaintext into the store's artifact encoding:
// plaintext for none, one concatenated-frame container for zstd.
func (st *Store) encodeArtifact(plaintext []byte) ([]byte, error) {
	if st.suffix() != CompressionZstd {
		return plaintext, nil
	}
	return compressZstdFrame(plaintext)
}

// decodeArtifact reads the artifact encoding back to plaintext. tornAt is
// the FILE truncation offset for crash repair: the end of the last complete
// frame (zstd) or the plaintext committed position (none); torn reports
// whether a torn tail exists at all.
func (st *Store) decodeArtifact(container []byte) (plaintext []byte, tornAt int64, torn bool, err error) {
	if st.suffix() != CompressionZstd {
		scan, err := ScanLog(container)
		if err != nil {
			return nil, 0, false, err
		}
		tornAt = scan.CommittedBytes
		return container, tornAt, tornAt < int64(len(container)), nil
	}
	scan, err := scanZstdFrames(container)
	if err != nil {
		return nil, 0, false, err
	}
	if scan.torn {
		// The torn final frame is dropped: its batch was never
		// acknowledged by the write-behind coordinator, which re-flushes
		// retained events after repair.
		tornAt = scan.tornStart
		if tornAt == 0 {
			return nil, 0, true, nil
		}
		container = container[:scan.tornStart]
	} else {
		tornAt = int64(len(container))
	}
	plaintext, err = decodeZstdFrames(container)
	if err != nil {
		return nil, 0, false, err
	}
	return plaintext, tornAt, scan.torn, nil
}

// PathOf is one session's artifact path.
func (st *Store) PathOf(cwd, id string) string {
	return LogPath(st.Root, cwd, id, st.suffix())
}

// Create writes the header line plus the seed events as a new artifact,
// refusing to overwrite an existing log.
func (st *Store) Create(header session.SessionHeader, seed []session.Event) error {
	// Every existing generation reserves the Session id, independently of
	// header readability (official create semantics).
	dir := SessionDir(st.Root, header.CWD, string(header.ID))
	suffix := LogSuffix(st.suffix())
	if entries, err := os.ReadDir(dir); err == nil {
		for _, entry := range entries {
			name := entry.Name()
			if !strings.HasSuffix(name, suffix) {
				continue
			}
			if _, canonical := parseGenerationFilename(strings.TrimSuffix(name, suffix)); canonical {
				return fmt.Errorf("session %q already has a log at %s", header.ID, filepath.Join(dir, name))
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	path := st.PathOf(header.CWD, string(header.ID))
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("session %q already has a log at %s", header.ID, path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var buffer bytes.Buffer
	headerJSON, err := json.Marshal(toHeaderLine(header))
	if err != nil {
		return err
	}
	buffer.Write(headerJSON)
	buffer.WriteByte('\n')
	lines, err := session.EventLines(seed, true)
	if err != nil {
		return err
	}
	if len(lines) > 0 {
		buffer.Write(lines)
		buffer.WriteByte('\n')
	}
	artifact, err := st.encodeArtifact(buffer.Bytes())
	if err != nil {
		return err
	}
	return os.WriteFile(path, artifact, 0o644)
}

// Load scans one session artifact without mutating it. A torn tail is
// reported on the scan; the caller repairs through CommitRepair. The
// returned CommittedBytes is where the next append resumes.
func (st *Store) Load(cwd, id string) (LogScan, error) {
	path := st.PathOf(cwd, id)
	container, err := os.ReadFile(path)
	if err != nil {
		return LogScan{}, err
	}
	plaintext, _, _, err := st.decodeArtifact(container)
	if err != nil {
		return LogScan{}, err
	}
	return ScanLog(plaintext)
}

// Append serializes an event batch and appends it after the committed
// prefix: plaintext lines (none) or one complete frame (zstd).
func (st *Store) Append(cwd, id string, committedBytes int64, events []session.Event) error {
	lines, err := session.EventLines(events, true)
	if err != nil {
		return err
	}
	if len(lines) == 0 {
		return nil
	}
	path := st.PathOf(cwd, id)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	var batch []byte
	if st.suffix() == CompressionZstd {
		batch, err = compressZstdFrame(append(append([]byte{}, lines...), '\n'))
		if err != nil {
			return err
		}
	} else {
		batch = append(append([]byte{}, lines...), '\n')
	}
	if _, err := file.Write(batch); err != nil {
		return err
	}
	return file.Sync()
}

// List walks every project directory and reports each log's header without
// parsing event rows.
func (st *Store) List() ([]session.SessionHeader, error) {
	entries, err := os.ReadDir(st.Root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var headers []session.SessionHeader
	for _, project := range entries {
		if !project.IsDir() {
			continue
		}
		sessions, err := os.ReadDir(filepath.Join(st.Root, project.Name()))
		if err != nil {
			continue
		}
		for _, dir := range sessions {
			if !dir.IsDir() {
				continue
			}
			path, _, found := SelectGenerationLog(filepath.Join(st.Root, project.Name(), dir.Name()), st.suffix())
			if !found {
				continue
			}
			container, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			plaintext, _, _, decodeErr := st.decodeArtifact(container)
			if decodeErr != nil {
				continue
			}
			scanner := bufio.NewScanner(bytes.NewReader(plaintext))
			scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
			if !scanner.Scan() {
				continue
			}
			header, ok, metaErr := ParseHeaderMeta(scanner.Text())
			if metaErr != nil {
				return nil, metaErr
			}
			if ok {
				headers = append(headers, header)
			}
		}
	}
	return headers, nil
}

// Package credentials re-implements the credential-reference capability seam
// of @deepseek-ai/dsh-credentials (official tag dsh-v0.1.2-alpha.1): two
// disjoint key spaces answer two questions. A Ref — a POSIX-style
// environment-variable name — resolves per operation through layered sources
// (the local provider uses env, file, project-env, and user-env). A Key —
// `<scope>/<id>` owned by the registering plugin — addresses stored records;
// nothing layers there, so presence of the record is the whole fact and
// ModifyRecord is the only write path.
//
// One seam-wide rule binds the reference half: an empty stored value is
// absent everywhere — resolution skips it and Describe reports it
// unconfigured — so a blank never masquerades as a configured secret.
//
// Deviation note: the official seam fans committed changes out through
// cordis events (`credentials/reference-updated`, `credentials/record-updated`).
// The Go port keeps the same containment contract on a typed listener
// registry (Notifier); bridging onto a shared event bus happens at the
// SDK/event layer.
package credentials

import (
	"errors"
	"fmt"
	"regexp"
	"sync"

	"dshgo/cordis"
)

var (
	refPattern        = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	keySegmentPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
)

// Ref is a validated reference to one credential: an environment-variable
// name such as DEEPSEEK_API_KEY.
type Ref string

// Key is a validated stored-record address `<scope>/<id>`. The scope is the
// owning plugin's registered name; the id is that plugin's own addressing
// unit. The `/` keeps the grammar disjoint from Ref, so the two key spaces
// can never collide.
type Key string

// Record discriminants.
const (
	KindAPIKey = "api-key"
	KindGrant  = "grant"
)

// CredentialRef brands a raw string as a Ref. A POSIX shell identifier such
// as DEEPSEEK_API_KEY is accepted; everything else fails loudly.
func CredentialRef(value string) (Ref, error) {
	if !IsCredentialRefName(value) {
		return "", fmt.Errorf("credentials: credential ref %q must match %s", value, refPattern.String())
	}
	return Ref(value), nil
}

// IsCredentialRefName reports whether a raw string could name a reference at
// all. A name outside the grammar has no reference to miss and reads as
// "not set" rather than as an error.
func IsCredentialRefName(value string) bool {
	return refPattern.MatchString(value)
}

// IsCredentialKeySegment reports whether a raw string could be a Key segment
// at all. A unit outside the grammar can never have stored a record and reads
// as "nothing stored" rather than as an error.
func IsCredentialKeySegment(value string) bool {
	return keySegmentPattern.MatchString(value)
}

// CredentialKey brands a scope and an id as a Key.
func CredentialKey(scope, id string) (Key, error) {
	for _, segment := range []string{scope, id} {
		if !keySegmentPattern.MatchString(segment) {
			return "", fmt.Errorf("credentials: credential key segment %q must match %s", segment, keySegmentPattern.String())
		}
	}
	return Key(scope + "/" + id), nil
}

// ParseCredentialKey brands a stored `<scope>/<id>` string as a Key — the
// read half of CredentialKey, for a provider admitting keys off disk.
func ParseCredentialKey(value string) (Key, error) {
	segments := splitKey(value)
	if len(segments) != 2 {
		return "", fmt.Errorf("credentials: credential key %q must be \"<scope>/<id>\"", value)
	}
	return CredentialKey(segments[0], segments[1])
}

// splitKey splits on the FIRST slash only; a second slash fails segment
// validation in CredentialKey rather than changing the split.
func splitKey(value string) []string {
	for i := 0; i < len(value); i++ {
		if value[i] == '/' {
			return []string{value[:i], value[i+1:]}
		}
	}
	return []string{value}
}

// CredentialKeyScope returns the owning plugin's name for one key. A record
// whose scope names no currently registered owner is an orphan, which a
// configuration surface must report as such rather than as a working
// credential.
func CredentialKeyScope(key Key) string {
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			return string(key[:i])
		}
	}
	return string(key)
}

// CredentialKeyID returns the owning plugin's own addressing unit for one key.
func CredentialKeyID(key Key) string {
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			return string(key[i+1:])
		}
	}
	return ""
}

// Record is one durable credential record, tagged by what the seam may do
// with it — the Go rendering of the official ApiKeyRecord | GrantRecord
// union. It survives a JSON round trip unchanged.
type Record struct {
	// Kind discriminates: KindAPIKey or KindGrant.
	Kind string `json:"kind"`
	// Key is the non-empty secret value for api-key records, when this
	// credential is a key at all.
	Key string `json:"key,omitempty"`
	// Env holds provider environment values such as AWS_PROFILE; api-key
	// records only. Names are POSIX identifiers.
	Env map[string]string `json:"env,omitempty"`
	// Payload is the owner-defined JSON value of a grant record, opaque to
	// the seam and to every other plugin.
	Payload any `json:"payload,omitempty"`
}

// Resolved is one resolved credential value and the source layer that
// supplied it.
type Resolved struct {
	Value  string
	Source string
}

// Info carries source and writability facts for one reference, safe for
// configuration surfaces — never the value. An empty Source means
// unconfigured.
type Info struct {
	Configured bool
	Source     string
	Writable   bool
}

// RecordInfo carries presence and writability facts for one record. Unlike a
// reference, presence alone answers configured: an api-key record carrying
// neither a key nor environment values states that its owner confirmed
// ambient authentication, which is configured, not blank.
type RecordInfo struct {
	Configured bool
	Kind       string
	Writable   bool
}

// RecordEntry is one stored record's address and tag, for enumeration —
// never its value.
type RecordEntry struct {
	Key  Key
	Kind string
}

// Provider is the abstract credential service over the two key spaces.
// Resolution is per call: consumers re-resolve at each operation and must not
// cache across operations. All writes commit before Notifier fans out, so a
// broken observer can never make a durable change look failed.
type Provider interface {
	// Resolve resolves one reference to its current value. A nil result with
	// a nil error means unconfigured.
	Resolve(ref Ref) (*Resolved, error)
	// Describe describes one reference without exposing the value.
	Describe(ref Ref) (Info, error)
	// Set durably stores one non-empty value in the provider-managed
	// writable source; it refuses an empty value (use Unset) and refuses to
	// write while a read-only source shadows the reference.
	Set(ref Ref, value string) error
	// Unset removes one reference; removing an absent reference is a no-op.
	Unset(ref Ref) error
	// ReadRecord reads one stored record as its owner wrote it; false means
	// none is stored.
	ReadRecord(key Key) (Record, bool, error)
	// DescribeRecord describes one record without exposing its value.
	DescribeRecord(key Key) (RecordInfo, error)
	// ListRecords enumerates every stored record's address and tag.
	ListRecords() ([]RecordEntry, error)
	// ModifyRecord runs one serialized read-modify-write over a record — the
	// only record write path, because a correct write (a token refresh) is
	// read-decide-replace under one lock. The mutate callback sees the
	// record as it stands when the write is exclusive; returning nil leaves
	// the entry untouched and reports the current record.
	ModifyRecord(key Key, mutate func(current *Record) *Record) (*Record, error)
	// DeleteRecord removes one record; removing an absent record is a no-op.
	DeleteRecord(key Key) error
}

// InvariantCode marks listener failures that must rethrow after every
// listener ran.
const InvariantCode = "INVARIANT"

// CodedError carries a seam error code.
type CodedError interface {
	error
	ErrorCode() string
}

type codedError struct {
	code string
	err  error
}

func (e *codedError) Error() string     { return e.err.Error() }
func (e *codedError) ErrorCode() string { return e.code }
func (e *codedError) Unwrap() error     { return e.err }

// NewCodedError tags an error with a seam code.
func NewCodedError(code string, err error) error {
	return &codedError{code: code, err: err}
}

// Notifier fans committed changes out to listeners with the official
// containment: every listener runs; a synchronous failure — an error return
// or a panic — is logged without changing the committed operation's outcome;
// an INVARIANT-coded failure rethrows after every listener ran.
type Notifier struct {
	logger cordis.Logger

	mu        sync.Mutex
	listeners []listenerEntry
	nextID    uint64
}

type listenerEntry struct {
	id uint64
	fn func(subject string) error
}

// NewNotifier builds a notifier; a nil logger discards records.
func NewNotifier(logger cordis.Logger) *Notifier {
	if logger == nil {
		logger = cordis.Discard{}
	}
	return &Notifier{logger: logger}
}

// On registers a listener and returns its disposer.
func (n *Notifier) On(fn func(subject string) error) cordis.Disposer {
	n.mu.Lock()
	n.nextID++
	id := n.nextID
	n.listeners = append(n.listeners, listenerEntry{id: id, fn: fn})
	n.mu.Unlock()
	return func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		for i, entry := range n.listeners {
			if entry.id == id {
				n.listeners = append(n.listeners[:i], n.listeners[i+1:]...)
				return
			}
		}
	}
}

// FanOut dispatches subject to every listener. The first INVARIANT-coded
// failure is remembered, never swallowed, and rethrown after every listener
// ran.
func (n *Notifier) FanOut(event, subject string) error {
	n.mu.Lock()
	listeners := make([]func(string) error, len(n.listeners))
	for i, entry := range n.listeners {
		listeners[i] = entry.fn
	}
	n.mu.Unlock()

	var invariantFailure error
	for _, fn := range listeners {
		n.runListener(event, subject, fn, &invariantFailure)
	}
	return invariantFailure
}

func (n *Notifier) runListener(event, subject string, fn func(string) error, invariantFailure *error) {
	err := func() (err error) {
		defer func() {
			if rec := recover(); rec != nil {
				if coded, ok := rec.(CodedError); ok {
					err = coded
					return
				}
				err = fmt.Errorf("listener panicked: %v", rec)
			}
		}()
		return fn(subject)
	}()
	if err == nil {
		return
	}
	var coded CodedError
	if errors.As(err, &coded) && coded.ErrorCode() == InvariantCode && *invariantFailure == nil {
		*invariantFailure = err
		return
	}
	n.logger.Warn(fmt.Sprintf("credentials: a %s listener for %q failed", event, subject))
	n.logger.Warn(err.Error())
}

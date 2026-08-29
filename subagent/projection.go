package subagent

import (
	"bytes"
	"encoding/json"
	"fmt"

	"dshgo/session"
	"dshgo/session/projection"
)

// Pure session projections for subagent identity (mode/label) and
// active-turn duration (official projection.ts). Both units are registered
// into the shared projection registry; their wire views are the client
// values the persisted cache and the listing read.

// Projection unit keys.
const (
	ProjectionKeySubagent       = "subagent"
	ProjectionKeySubagentTiming = "subagentTiming"
)

// TimingInterval bounds one open turn.
type TimingInterval struct {
	Since   int64 `json:"since"`
	Through int64 `json:"through"`
}

// timingState is the fold state for a subagent's latest timing snapshot.
type timingState struct {
	// SettledMs accumulates completed post-descriptor turns.
	SettledMs int64 `json:"settledMs"`
	// Active is the current open interval, kept paired inside the fold.
	Active *TimingInterval `json:"active,omitempty"`
	// PendingTurnStart is the latest pre-descriptor turn start, promoted
	// when the child's own descriptor arrives.
	PendingTurnStart *int64 `json:"pendingTurnStart,omitempty"`
	// DescriptorSeen reports whether the fold crossed a descriptor in this
	// logical log.
	DescriptorSeen bool `json:"descriptorSeen"`
}

// identityState carries the identity from the last valid descriptor; absent
// (nil pointer) before one, and after an invalid one.
type identityState struct {
	Identity *SubagentIdentityProjection `json:"identity,omitempty"`
}

// subagentTimingProjection folds turn boundaries around the child's own
// durable descriptor. A fork seed may contain an ancestor descriptor and
// completed turns; every descriptor therefore resets the accumulated state,
// and the healthy catalog admits only a child with exactly one descriptor in
// its own suffix, making the final reset the child's authoritative timing
// origin.
// subagentTimingUnit is the typed unit; subagentTimingProjection its
// erased runtime record for registry registration.
var subagentTimingUnit = projection.Unit[*timingState]{
	Key:          ProjectionKeySubagentTiming,
	StateVersion: 2,
	Init: func(session.SessionHeader) *timingState {
		return &timingState{}
	},
	Apply: func(current *timingState, event session.Event) (*timingState, bool) {
		switch event.Type {
		case session.EventTurnStart:
			if current.DescriptorSeen {
				return &timingState{
					SettledMs:      current.SettledMs,
					DescriptorSeen: true,
					Active:         &TimingInterval{Since: event.Time, Through: event.Time},
				}, true
			}
			start := event.Time
			return &timingState{
				SettledMs:        current.SettledMs,
				PendingTurnStart: &start,
				DescriptorSeen:   false,
			}, true
		case EventSubagentDescriptor:
			next := &timingState{DescriptorSeen: true}
			var activeSince *int64
			if current.Active != nil {
				activeSince = &current.Active.Since
			} else {
				activeSince = current.PendingTurnStart
			}
			if activeSince != nil {
				next.Active = &TimingInterval{Since: *activeSince, Through: event.Time}
			}
			return next, true
		case session.EventTurnEnd:
			if !current.DescriptorSeen {
				if current.PendingTurnStart == nil {
					return current, false
				}
				return &timingState{SettledMs: current.SettledMs}, true
			}
			if current.Active == nil {
				return current, false
			}
			return &timingState{
				SettledMs:      current.SettledMs + maxInt64(0, event.Time-current.Active.Since),
				DescriptorSeen: true,
			}, true
		default:
			if current.Active == nil {
				return current, false
			}
			return &timingState{
				SettledMs:        current.SettledMs,
				Active:           &TimingInterval{Since: current.Active.Since, Through: event.Time},
				PendingTurnStart: current.PendingTurnStart,
				DescriptorSeen:   current.DescriptorSeen,
			}, true
		}
	},
	View: func(timing *timingState) any {
		view := SubagentTimingProjection{SettledMs: timing.SettledMs}
		if timing.Active != nil {
			view.Active = &SubagentTimingActive{Since: timing.Active.Since, Through: timing.Active.Through}
		}
		return view
	},
	DecodeState: decodeTimingState,
}

var subagentTimingProjection = subagentTimingUnit.Definition()

// subagentIdentityProjection folds the durable mode/label identity from
// `subagent/descriptor` events, last-wins: a fork seed may replay an
// ancestor's descriptor, and the child's own descriptor must override it —
// the same reset discipline as the timing unit. A malformed or
// unknown-version payload resets to the nil sentinel instead of throwing, so
// a fork of a healthy ancestor never inherits an identity its own descriptor
// failed to establish.
var subagentIdentityUnit = projection.Unit[*identityState]{
	Key:          ProjectionKeySubagent,
	StateVersion: 2,
	Init: func(session.SessionHeader) *identityState {
		return &identityState{}
	},
	Apply: func(current *identityState, event session.Event) (*identityState, bool) {
		if event.Type != EventSubagentDescriptor {
			return current, false
		}
		identity := descriptorIdentity(event)
		if identity == nil {
			return &identityState{}, true
		}
		return &identityState{Identity: identity}, true
	},
	// The view returns the serializable nil sentinel — never an absent
	// key — so every registry read and push frame survives JSON
	// losslessly and a consumer holding an earlier identity replaces it
	// instead of keeping it stale.
	View: func(state *identityState) any {
		if state.Identity == nil {
			return nil
		}
		return state.Identity
	},
	DecodeState: decodeIdentityState,
}

var subagentIdentityProjection = subagentIdentityUnit.Definition()

// descriptorIdentity interprets one `subagent/descriptor` event's identity;
// nil when the payload cannot be trusted. Only a malformed current-version
// payload throws in descriptor parsing; a projection fold must never throw,
// so damage folds to no value.
func descriptorIdentity(event session.Event) *SubagentIdentityProjection {
	descriptor, err := FoldSubagentDescriptor([]session.Event{event})
	if err != nil || descriptor == nil {
		return nil
	}
	identity := &SubagentIdentityProjection{Mode: descriptor.Mode, Label: descriptor.Label, Seq: event.Seq}
	return identity
}

// RegisterSubagentProjections adds both units to a projection registry. The
// returned disposer unregisters this registration site.
func RegisterSubagentProjections(registry *projection.Registry) (func(), error) {
	undoTiming, err := registry.Register(subagentTimingProjection)
	if err != nil {
		return nil, err
	}
	undoIdentity, err := registry.Register(subagentIdentityProjection)
	if err != nil {
		undoTiming()
		return nil, err
	}
	return func() { undoIdentity(); undoTiming() }, nil
}

// decodeTimingState validates and reifies a persisted timing row (strict:
// unknown fields reject, matching the official .strict() state schema).
func decodeTimingState(raw json.RawMessage) (*timingState, error) {
	var state timingState
	if err := strictUnmarshal(raw, &state); err != nil {
		return nil, err
	}
	if state.SettledMs < 0 {
		return nil, fmt.Errorf("settledMs must be a non-negative integer")
	}
	if state.Active != nil && (state.Active.Since < 0 || state.Active.Through < 0) {
		return nil, fmt.Errorf("active interval bounds must be non-negative integers")
	}
	if state.PendingTurnStart != nil && *state.PendingTurnStart < 0 {
		return nil, fmt.Errorf("pendingTurnStart must be a non-negative integer")
	}
	return &state, nil
}

// decodeIdentityState validates and reifies a persisted identity row.
func decodeIdentityState(raw json.RawMessage) (*identityState, error) {
	var state identityState
	if err := strictUnmarshal(raw, &state); err != nil {
		return nil, err
	}
	if state.Identity != nil {
		if state.Identity.Mode != ModeOneShot && state.Identity.Mode != ModeContinuable {
			return nil, fmt.Errorf("identity mode %q is outside the closed union", state.Identity.Mode)
		}
		if state.Identity.Mode == ModeContinuable && state.Identity.Label == nil {
			return nil, fmt.Errorf("a continuable identity always carries a label")
		}
		if state.Identity.Seq < 0 {
			return nil, fmt.Errorf("seq must be a non-negative integer")
		}
	}
	return &state, nil
}

// strictUnmarshal decodes JSON rejecting unknown fields.
func strictUnmarshal(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.More() {
		return fmt.Errorf("trailing data after projection state")
	}
	return nil
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

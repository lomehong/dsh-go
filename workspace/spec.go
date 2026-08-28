// Package workspace ports packages/workspace/workspace: stable workspace
// records over existing directories. This round ports the domain-independent
// core — the record/state durable shapes, path canonicalization, and the
// entity write path — through the same host seams the source entity sees
// (the open table, the canonical session-path index, attach-time header
// reads). The registry bootstrap/create/delete transaction layer lands with
// the storage-domain round; its spec below is already the durable contract.
package workspace

import (
	"encoding/json"
	"fmt"
)

// WorkspaceID identifies one workspace record: a generated uuid, never the
// path — path normalization rewrites paths, and a reference anchor must stay
// stable.
type WorkspaceID = string

// WorkspaceRecord is the durable shape of one workspace record. Path is the
// realpath canon stamped at create; SessionIDs is the ordered ownership
// account (array order is display order); timestamps are ISO-8601 strings.
type WorkspaceRecord struct {
	Path       string   `json:"path"`
	Title      string   `json:"title"`
	SessionIDs []string `json:"sessionIds"`
	CreatedAt  string   `json:"createdAt"`
	UpdatedAt  string   `json:"updatedAt"`
}

// ValidateWorkspaceRecord enforces the shipped record format at the
// durability boundary (the zod schema's runtime half): all five fields
// present with the right kinds.
func ValidateWorkspaceRecord(data []byte) (WorkspaceRecord, error) {
	var record WorkspaceRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return WorkspaceRecord{}, fmt.Errorf("workspace record: %w", err)
	}
	if record.Path == "" {
		return WorkspaceRecord{}, fmt.Errorf("workspace record: path must be a string")
	}
	if record.Title == "" {
		return WorkspaceRecord{}, fmt.Errorf("workspace record: title must be a string")
	}
	if record.SessionIDs == nil {
		return WorkspaceRecord{}, fmt.Errorf("workspace record: sessionIds must be an array")
	}
	if record.CreatedAt == "" || record.UpdatedAt == "" {
		return WorkspaceRecord{}, fmt.Errorf("workspace record: createdAt/updatedAt must be ISO-8601 strings")
	}
	return record, nil
}

// pendingMutation is the recoverable two-write mutation marker: persisted
// before the record/order pair can diverge, so startup can distinguish an
// interrupted registry operation from unexplained medium corruption.
type pendingMutation struct {
	Operation   string      `json:"operation"`
	WorkspaceID WorkspaceID `json:"workspaceId"`
}

// DomainState is the durable registry state. Initialized distinguishes a
// valid empty registry from one that still needs the header-only history
// bootstrap; WorkspaceIDs is the authoritative display order.
// ArchivedSessionIDs is the registry-global archive set layered over
// workspace accounting: an archived session keeps its sessionIds slot
// (unarchiving must restore the position), so the set never participates in
// the one-owner accounting invariant. PendingMutation is the optional
// recoverable marker.
type DomainState struct {
	Initialized        bool             `json:"initialized"`
	WorkspaceIDs       []WorkspaceID    `json:"workspaceIds"`
	ArchivedSessionIDs []string         `json:"archivedSessionIds"`
	PendingMutation    *pendingMutation `json:"pendingMutation,omitempty"`
}

// InitialDomainState is the state a fresh registry starts from (the domain
// spec's `initial`).
func InitialDomainState() DomainState {
	return DomainState{Initialized: false, WorkspaceIDs: []WorkspaceID{}, ArchivedSessionIDs: []string{}}
}

// ValidateDomainState enforces the durable registry state shape: the
// discriminated pending-mutation union admits only the create/delete
// operations with a workspace id.
func ValidateDomainState(data []byte) (DomainState, error) {
	var state DomainState
	if err := json.Unmarshal(data, &state); err != nil {
		return DomainState{}, fmt.Errorf("workspace domain state: %w", err)
	}
	if state.WorkspaceIDs == nil {
		return DomainState{}, fmt.Errorf("workspace domain state: workspaceIds must be an array")
	}
	if state.ArchivedSessionIDs == nil {
		// Defaulted so records written before the field parse unchanged.
		state.ArchivedSessionIDs = []string{}
	}
	if state.PendingMutation != nil {
		switch state.PendingMutation.Operation {
		case "create", "delete":
			if state.PendingMutation.WorkspaceID == "" {
				return DomainState{}, fmt.Errorf("workspace domain state: pendingMutation.%s requires a workspaceId", state.PendingMutation.Operation)
			}
		default:
			return DomainState{}, fmt.Errorf("workspace domain state: pendingMutation.operation must be create or delete")
		}
	}
	return state, nil
}

// DomainSpec is the workspace domain declaration: one `workspaces` table
// keyed by WorkspaceID plus the bootstrap/order singleton. The registry
// opens this through ctx.storage.domain; the spec is the single source of
// the domain's identity, version, and schemas.
type DomainSpec struct {
	Name    string
	Version int
	// InitialJSON is the serialized initial global state.
	InitialJSON string
}

// WorkspaceDomainSpec is the shipped spec (name `workspace`, version 2).
var WorkspaceDomainSpec = DomainSpec{
	Name:        "workspace",
	Version:     2,
	InitialJSON: `{"initialized":false,"workspaceIds":[],"archivedSessionIds":[]}`,
}

// Opaque revision identity for lightweight persistence observations.
// Port of revision.ts.
package persistence

// Revision is a backend-owned token that identifies both one storage source
// and one revision of a persisted session log.
type Revision string

// NewRevision brands a backend revision for the provider-neutral
// persistence contract.
func NewRevision(value string) Revision { return Revision(value) }

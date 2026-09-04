// Package storagedomain ports packages/storage/storage-domain: the domain
// data form — schema-validated, change-emitting KV domains over storage
// backends. Consumers depend on this package and never touch backends
// directly.
//
// Go adaptations (all documented at the member they replace): backends are
// synchronous interfaces (a Go file backend writes synchronously), so the
// source's per-domain async write chain collapses to mutex serialization
// with a WaitGroup drain for close; zod record schemas become
// ValidateRecord/ValidateGlobal functions run at the same durable read
// boundary.
package storagedomain

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// UnitNamePattern is the allowed format for unit and table names: safe as a
// file name and as a SQL identifier segment without escaping.
var UnitNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Layout names the medium layout of a unit.
const (
	// LayoutSingle keeps the whole unit in one document (the default).
	LayoutSingle = "single"
	// LayoutPerRecord keeps each record in its own document, so a unit whose
	// records are large or sparse never rewrites the rest on one write, and
	// a version bump discards stale records instead of rejecting the whole
	// unit.
	LayoutPerRecord = "per-record"
)

// InvalidRecordPolicy names how a stored record that fails its schema is
// handled at open.
const (
	// InvalidRecordsFailLoud (the default) rejects the whole open with
	// `invalid-record` — right for authoritative data.
	InvalidRecordsFailLoud = ""
	// InvalidRecordsBackupAndSkip moves the failing record's document aside
	// (through the backend's RecordBackuper seam), logs the cause, and opens
	// without the record — for domains whose records are disposable derived
	// data. Backends without the seam keep the fail-loud path.
	InvalidRecordsBackupAndSkip = "backup-and-skip"
)

// DomainSpec is the static declaration of one domain: identity, version, and
// record layout. The owning package defines it once with DefineDomain; both
// the type surface and the runtime (validation, descriptor projection)
// derive from it.
type DomainSpec struct {
	// Name is the domain name; it must match UnitNamePattern (it doubles as
	// the backend unit name).
	Name string
	// Version is the domain format version; a medium stamped with a
	// different version rejects at open.
	Version int
	// CompatibleVersions lists older versions whose stored records the
	// current record schemas still accept. Per-record reads admit documents
	// stamped with any listed version; the legacy whole-unit bootstrap
	// accepts only a legacy file stamped with one. Writes always stamp
	// Version. Single-layout reads stay exact-version.
	CompatibleVersions []int
	// InvalidRecordPolicy names how a stored record failing its schema is
	// handled at open: InvalidRecordsBackupAndSkip moves it aside and skips;
	// the default (InvalidRecordsFailLoud) rejects the open.
	InvalidRecordPolicy string
	// Layout is the medium layout; empty means LayoutSingle.
	Layout string
	// Tables are the declared table names, each matching UnitNamePattern.
	Tables []string
	// HasGlobal declares the global singleton slot.
	HasGlobal bool
	// InitialGlobalJSON is the value served when the medium holds no global
	// yet (null sentinel); not written until the first Set.
	InitialGlobalJSON json.RawMessage
	// ValidateRecord validates every stored record at the durable read
	// boundary (the zod schema's runtime half). Table/key identify the slot
	// for diagnostics.
	ValidateRecord func(table string, key string, raw json.RawMessage) error
	// ValidateGlobal validates the stored global at the durable read
	// boundary. It must reject a JSON `null`: null is the medium's "never
	// written" sentinel, so a nullable global could not round-trip.
	ValidateGlobal func(raw json.RawMessage) error
}

// DefineDomain validates a spec's fields. Misconfiguration fails loud at the
// owning package's module load, before any medium is touched: a domain or
// table name outside UnitNamePattern, a version that is not a non-negative
// integer, an unknown layout, or a global validator that accepts `null` all
// fail.
func DefineDomain(spec DomainSpec) (DomainSpec, error) {
	if !UnitNamePattern.MatchString(spec.Name) {
		return DomainSpec{}, fmt.Errorf("domain name '%s' must match %s", spec.Name, UnitNamePattern)
	}
	if spec.Version < 0 {
		return DomainSpec{}, fmt.Errorf("domain '%s' version must be a non-negative integer, got %d", spec.Name, spec.Version)
	}
	for _, compat := range spec.CompatibleVersions {
		if compat < 0 || compat >= spec.Version {
			return DomainSpec{}, fmt.Errorf(
				"domain '%s' compatibleVersions entries must be non-negative integers below version %d, got %d",
				spec.Name, spec.Version, compat)
		}
	}
	if spec.InvalidRecordPolicy != "" && spec.InvalidRecordPolicy != InvalidRecordsBackupAndSkip {
		return DomainSpec{}, fmt.Errorf("domain '%s' invalidRecordPolicy must be 'backup-and-skip' when present, got %s",
			spec.Name, spec.InvalidRecordPolicy)
	}
	if spec.Layout != "" && spec.Layout != LayoutSingle && spec.Layout != LayoutPerRecord {
		return DomainSpec{}, fmt.Errorf("domain '%s' layout must be 'single' or 'per-record', got %s", spec.Name, spec.Layout)
	}
	for _, table := range spec.Tables {
		if !UnitNamePattern.MatchString(table) {
			return DomainSpec{}, fmt.Errorf("domain '%s' table name '%s' must match %s", spec.Name, table, UnitNamePattern)
		}
	}
	if spec.HasGlobal && spec.ValidateGlobal != nil {
		if err := spec.ValidateGlobal(json.RawMessage(`null`)); err == nil {
			return DomainSpec{}, fmt.Errorf(
				"domain '%s' global schema must not accept null: "+
					"null is the medium's \"never written\" sentinel, so a stored null could not round-trip",
				spec.Name)
		}
	}
	return spec, nil
}

// KvUnitDescriptor is the static identity and shape of one KV unit,
// projected from its owner's spec.
type KvUnitDescriptor struct {
	// Name is the unit name; also the file-name / SQL-identifier segment.
	Name string
	// Version is the unit format version stamped on the medium at first
	// materialization.
	Version int
	// CompatibleVersions lists older versions the unit reads as its own
	// (acceptedStamps = Version + CompatibleVersions).
	CompatibleVersions []int
	// Tables are the table names.
	Tables []string
	// HasGlobal declares the global singleton slot.
	HasGlobal bool
	// Layout is the medium layout; empty means single.
	Layout string
}

// DescriptorOf projects a spec onto the backend-facing unit descriptor.
func DescriptorOf(spec DomainSpec) KvUnitDescriptor {
	return KvUnitDescriptor{
		Name:               spec.Name,
		Version:            spec.Version,
		CompatibleVersions: spec.CompatibleVersions,
		Tables:             spec.Tables,
		HasGlobal:          spec.HasGlobal,
		Layout:             spec.Layout,
	}
}

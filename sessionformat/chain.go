package sessionformat

import (
	"errors"
	"fmt"
)

// Pure adjacent migration planning and whole-artifact composition. Port of
// packages/session/session-format/src/chain.ts.

// Migration is one adjacent vN -> vN+1 conversion (official
// SessionFormatMigration).
type Migration interface {
	// Name is the exact adjacent conversion's stable identity.
	Name() string
	FromVersion() int64
	ToVersion() int64
	// MigrateHeader converts one header without reading event bodies.
	MigrateHeader(header Header) (Header, error)
	// Migrate converts one detached complete artifact to exactly ToVersion.
	Migrate(artifact Artifact) (Artifact, error)
	// ValidateTarget refuses any artifact the adjacent target writer cannot
	// emit.
	ValidateTarget(artifact Artifact) error
	// ValidateTargetHeader refuses any header the adjacent target writer
	// cannot emit.
	ValidateTargetHeader(header Header) error
}

// ChainOptions are the inputs that compile the unique complete migration
// chain.
type ChainOptions struct {
	CurrentVersion int64
	Migrations     []Migration
	// RestoreCurrent restores and validates a detached current artifact
	// through the current parser.
	RestoreCurrent func(Artifact) (Artifact, error)
	// RestoreCurrentHeader restores and validates a detached current header
	// without reading event bodies.
	RestoreCurrentHeader func(Header) (Header, error)
}

// Chain is the pure adjacent planner and whole-artifact migration runner.
type Chain interface {
	CurrentVersion() int64
	// Plan returns the complete ordered plan from one supported stored
	// version.
	Plan(fromVersion int64) ([]Migration, error)
	// Migrate restores current input directly or migrates old input
	// entirely in memory.
	Migrate(artifact Artifact) (Artifact, error)
	// MigrateHeader converts only a supported header to the current logical
	// representation.
	MigrateHeader(header Header) (Header, error)
}

// CompileChain validates and compiles one unique, complete adjacent
// migration chain (official createSessionFormatChain).
func CompileChain(options ChainOptions) (Chain, error) {
	if options.CurrentVersion < 0 {
		return nil, formatErrorf("Session format version must be a non-negative safe integer")
	}
	if options.RestoreCurrent == nil || options.RestoreCurrentHeader == nil {
		return nil, formatErrorf("current Session restorers are required")
	}
	byFrom := map[int64]Migration{}
	names := map[string]bool{}
	for _, candidate := range options.Migrations {
		if candidate.Name() == "" {
			return nil, formatErrorf("Session migration name must be a non-empty string")
		}
		from, to := candidate.FromVersion(), candidate.ToVersion()
		if from < 0 || to != from+1 {
			return nil, formatErrorf("%s must declare adjacent v%d->v%d", candidate.Name(), from, from+1)
		}
		if _, duplicate := byFrom[from]; duplicate {
			return nil, formatErrorf("Session migration v%d->v%d is duplicated", from, to)
		}
		if names[candidate.Name()] {
			return nil, formatErrorf("Session migration name %q is duplicated", candidate.Name())
		}
		byFrom[from] = candidate
		names[candidate.Name()] = true
	}
	ordered := make([]Migration, 0, options.CurrentVersion)
	for version := int64(0); version < options.CurrentVersion; version++ {
		migration, ok := byFrom[version]
		if !ok {
			return nil, unsupportedf("Session migration v%d->v%d is missing", version, version+1)
		}
		ordered = append(ordered, migration)
	}
	if len(byFrom) != len(ordered) {
		for version := range byFrom {
			if version >= options.CurrentVersion {
				return nil, formatErrorf("Session migration from v%d does not lead to current v%d", version, options.CurrentVersion)
			}
		}
	}
	return &compiledChain{
		currentVersion: options.CurrentVersion,
		migrations:     ordered,
		restoreCurrent: options.RestoreCurrent,
		restoreHeader:  options.RestoreCurrentHeader,
	}, nil
}

type compiledChain struct {
	currentVersion int64
	migrations     []Migration
	restoreCurrent func(Artifact) (Artifact, error)
	restoreHeader  func(Header) (Header, error)
}

func (c *compiledChain) CurrentVersion() int64 { return c.currentVersion }

func (c *compiledChain) Plan(fromVersion int64) ([]Migration, error) {
	if fromVersion < 0 {
		return nil, formatErrorf("stored Session format version must be a non-negative safe integer")
	}
	if fromVersion > c.currentVersion {
		return nil, unsupportedf("stored Session uses newer format v%d; this build writes v%d", fromVersion, c.currentVersion)
	}
	return c.migrations[fromVersion:], nil
}

func (c *compiledChain) Migrate(source Artifact) (Artifact, error) {
	storedVersion, err := HeaderVersion(source.Header)
	if err != nil {
		return Artifact{}, err
	}
	current, err := SnapshotArtifact(source, "format v"+itoa(storedVersion)+" source")
	if err != nil {
		return Artifact{}, err
	}
	if storedVersion == c.currentVersion {
		restored, err := c.restoreCurrent(current)
		if err != nil {
			return Artifact{}, err
		}
		current, err = SnapshotArtifact(restored, "current Session restoration")
		if err != nil {
			return Artifact{}, err
		}
		return c.assertCurrent(current)
	}
	plan, err := c.Plan(storedVersion)
	if err != nil {
		return Artifact{}, err
	}
	for _, migration := range plan {
		input, err := SnapshotArtifact(current, migration.Name()+" input")
		if err != nil {
			return Artifact{}, refusal(migration, "Session", err)
		}
		migrated, err := migration.Migrate(input)
		if err != nil {
			return Artifact{}, refusal(migration, "Session", err)
		}
		current, err = SnapshotArtifact(migrated, migration.Name()+" output")
		if err != nil {
			return Artifact{}, err
		}
		version, err := HeaderVersion(current.Header)
		if err != nil {
			return Artifact{}, err
		}
		if version != migration.ToVersion() {
			return Artifact{}, formatErrorf("%s returned v%d; expected v%d", migration.Name(), version, migration.ToVersion())
		}
		if err := migration.ValidateTarget(current); err != nil {
			return Artifact{}, refusal(migration, "Session", err)
		}
	}
	restored, err := c.restoreCurrent(current)
	if err != nil {
		return Artifact{}, err
	}
	current, err = SnapshotArtifact(restored, "current Session restoration")
	if err != nil {
		return Artifact{}, err
	}
	return c.assertCurrent(current)
}

func (c *compiledChain) MigrateHeader(source Header) (Header, error) {
	current, err := SnapshotHeader(source, "stored Session header")
	if err != nil {
		return nil, err
	}
	version, err := HeaderVersion(current)
	if err != nil {
		return nil, err
	}
	plan, err := c.Plan(version)
	if err != nil {
		return nil, err
	}
	for _, migration := range plan {
		input, err := SnapshotHeader(current, migration.Name()+" header input")
		if err != nil {
			return nil, refusal(migration, "Session header", err)
		}
		migrated, err := migration.MigrateHeader(input)
		if err != nil {
			return nil, refusal(migration, "Session header", err)
		}
		current, err = SnapshotHeader(migrated, migration.Name()+" header output")
		if err != nil {
			return nil, err
		}
		version, err := HeaderVersion(current)
		if err != nil {
			return nil, err
		}
		if version != migration.ToVersion() {
			return nil, formatErrorf("%s header returned v%d; expected v%d", migration.Name(), version, migration.ToVersion())
		}
		if err := migration.ValidateTargetHeader(current); err != nil {
			return nil, refusal(migration, "Session header", err)
		}
	}
	restored, err := c.restoreHeader(current)
	if err != nil {
		return nil, err
	}
	current, err = SnapshotHeader(restored, "current Session header restoration")
	if err != nil {
		return nil, err
	}
	version, err = HeaderVersion(current)
	if err != nil {
		return nil, err
	}
	if version != c.currentVersion {
		return nil, formatErrorf("current Session header restorer returned v%d; expected v%d", version, c.currentVersion)
	}
	return current, nil
}

func (c *compiledChain) assertCurrent(artifact Artifact) (Artifact, error) {
	version, err := HeaderVersion(artifact.Header)
	if err != nil {
		return Artifact{}, err
	}
	if version != c.currentVersion {
		return Artifact{}, formatErrorf("current Session restorer returned v%d; expected v%d", version, c.currentVersion)
	}
	return artifact, nil
}

// refusal wraps an edge failure into an UnsupportedMigrationError unless the
// edge already refused directly (official throwUnsupportedRefusal).
func refusal(migration Migration, subject string, cause error) error {
	var unsupported *UnsupportedMigrationError
	if errors.As(cause, &unsupported) {
		return cause
	}
	return unsupportedf("%s refuses this format v%d %s: %s", migration.Name(), migration.FromVersion(), subject, cause.Error())
}

func itoa(value int64) string {
	return fmt.Sprintf("%d", value)
}

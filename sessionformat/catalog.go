package sessionformat

import (
	"encoding/json"
	"errors"
)

// Build-static physical codec and adjacent migration catalog. Port of
// packages/session/session-format/src/catalog.ts.

// Codec is one format generation's physical JSON codec (official
// SessionFormatCodec).
type Codec interface {
	Version() int64
	DecodeHeader(headerValue json.RawMessage) (Header, error)
	DecodeArtifact(headerValue json.RawMessage, rows []json.RawMessage) (Artifact, error)
	// DecodeRecoverableArtifact decodes the longest recoverable logical
	// prefix of a possibly torn physical artifact; the first issue aborts at
	// the next turn boundary instead of failing the whole read.
	DecodeRecoverableArtifact(headerValue json.RawMessage, rows []json.RawMessage) (Artifact, error)
}

// EncodedArtifact is one physically encoded current artifact: the header
// JSON plus one physical row per event.
type EncodedArtifact struct {
	Header json.RawMessage
	Rows   []json.RawMessage
}

// CatalogOptions compile the catalog.
type CatalogOptions struct {
	Codecs         []Codec
	Migrations     []Migration
	CurrentVersion int64
	// RestoreCurrent restores and validates a detached current artifact
	// through the current parser.
	RestoreCurrent func(Artifact) (Artifact, error)
	// RestoreCurrentHeader restores and validates a detached current header
	// without reading event bodies.
	RestoreCurrentHeader func(Header) (Header, error)
	// EncodeCurrentArtifact physically encodes one current artifact
	// (official encodeCurrentArtifact).
	EncodeCurrentArtifact func(Artifact) (EncodedArtifact, error)
}

// HeaderReadStatus classifies one physical header read.
type HeaderReadStatus string

// Header read statuses.
const (
	HeaderCurrent           HeaderReadStatus = "current"
	HeaderMigrationRequired HeaderReadStatus = "migration-required"
	HeaderUnsupported       HeaderReadStatus = "unsupported"
	HeaderMalformed         HeaderReadStatus = "malformed"
)

// HeaderReadResult is one directional header dispatch result.
type HeaderReadResult struct {
	Status        HeaderReadStatus
	StoredVersion int64
	// HasStoredVersion distinguishes a malformed value that never carried a
	// readable version.
	HasStoredVersion bool
	TargetVersion    int64
	Header           Header
	Reason           string
}

// Catalog is the immutable physical dispatch and migration operations face
// (official SessionFormatCatalog).
type Catalog interface {
	CurrentVersion() int64
	ReadHeader(headerValue json.RawMessage) (HeaderReadResult, error)
	DecodeArtifact(headerValue json.RawMessage, rows []json.RawMessage) (Artifact, error)
	DecodeRecoverableArtifact(headerValue json.RawMessage, rows []json.RawMessage) (Artifact, error)
	Migrate(artifact Artifact) (Artifact, error)
	// EncodeCurrent physically encodes exactly a current artifact.
	EncodeCurrent(artifact Artifact) (EncodedArtifact, error)
}

// CompileCatalog builds one build-static physical dispatch and migration
// catalog (official createSessionFormatCatalog): every version 0..current
// must carry exactly one codec, and the adjacent migration chain must be
// complete.
func CompileCatalog(options CatalogOptions) (Catalog, error) {
	chain, err := CompileChain(ChainOptions{
		CurrentVersion:       options.CurrentVersion,
		Migrations:           options.Migrations,
		RestoreCurrent:       options.RestoreCurrent,
		RestoreCurrentHeader: options.RestoreCurrentHeader,
	})
	if err != nil {
		return nil, err
	}
	codecs := map[int64]Codec{}
	for _, codec := range options.Codecs {
		version := codec.Version()
		if version < 0 {
			return nil, formatErrorf("Session format codec version must be a non-negative safe integer")
		}
		if _, duplicate := codecs[version]; duplicate {
			return nil, formatErrorf("Session format codec v%d is duplicated", version)
		}
		codecs[version] = codec
	}
	for version := int64(0); version <= chain.CurrentVersion(); version++ {
		if _, ok := codecs[version]; !ok {
			return nil, formatErrorf("Session format codec v%d is missing", version)
		}
	}
	if len(codecs) != int(chain.CurrentVersion())+1 {
		for version := range codecs {
			if version > chain.CurrentVersion() {
				return nil, formatErrorf("Session format codec v%d is newer than current v%d", version, chain.CurrentVersion())
			}
		}
	}
	if options.EncodeCurrentArtifact == nil {
		return nil, formatErrorf("current Session encoder is required")
	}
	return &compiledCatalog{
		chain:                 chain,
		codecs:                codecs,
		encodeCurrentArtifact: options.EncodeCurrentArtifact,
	}, nil
}

type compiledCatalog struct {
	chain                 Chain
	codecs                map[int64]Codec
	encodeCurrentArtifact func(Artifact) (EncodedArtifact, error)
}

func (c *compiledCatalog) CurrentVersion() int64 { return c.chain.CurrentVersion() }

func (c *compiledCatalog) ReadHeader(headerValue json.RawMessage) (HeaderReadResult, error) {
	target := c.chain.CurrentVersion()
	storedVersion, err := InspectVersion(headerValue)
	if err != nil {
		return HeaderReadResult{
			Status:        HeaderMalformed,
			TargetVersion: target,
			Reason:        err.Error(),
		}, nil
	}
	if storedVersion > target {
		return HeaderReadResult{
			Status:           HeaderUnsupported,
			StoredVersion:    storedVersion,
			HasStoredVersion: true,
			TargetVersion:    target,
			Reason:           unsupportedReason(storedVersion, target),
		}, nil
	}
	codec, ok := c.codecs[storedVersion]
	if !ok {
		return HeaderReadResult{
			Status:           HeaderUnsupported,
			StoredVersion:    storedVersion,
			HasStoredVersion: true,
			TargetVersion:    target,
			Reason:           "this build has no Session format codec for v" + itoa(storedVersion),
		}, nil
	}
	decodedHeader, err := codec.DecodeHeader(headerValue)
	if err != nil {
		return malformedResult(target, storedVersion, err), nil
	}
	snapshotted, err := SnapshotHeader(decodedHeader, "format v"+itoa(storedVersion)+" header")
	if err != nil {
		return malformedResult(target, storedVersion, err), nil
	}
	header, err := c.chain.MigrateHeader(snapshotted)
	if err != nil {
		var unsupported *UnsupportedMigrationError
		if asUnsupported(err, &unsupported) {
			return HeaderReadResult{
				Status:           HeaderUnsupported,
				StoredVersion:    storedVersion,
				HasStoredVersion: true,
				TargetVersion:    target,
				Reason:           err.Error(),
			}, nil
		}
		return malformedResult(target, storedVersion, err), nil
	}
	status := HeaderMigrationRequired
	if storedVersion == target {
		status = HeaderCurrent
	}
	return HeaderReadResult{
		Status:           status,
		StoredVersion:    storedVersion,
		HasStoredVersion: true,
		TargetVersion:    target,
		Header:           header,
	}, nil
}

func (c *compiledCatalog) artifactCodec(headerValue json.RawMessage) (int64, Codec, error) {
	storedVersion, err := InspectVersion(headerValue)
	if err != nil {
		return 0, nil, err
	}
	if storedVersion > c.chain.CurrentVersion() {
		return 0, nil, unsupportedf("%s", unsupportedReason(storedVersion, c.chain.CurrentVersion()))
	}
	codec, ok := c.codecs[storedVersion]
	if !ok {
		return 0, nil, unsupportedf("this build has no Session format codec for v%d", storedVersion)
	}
	return storedVersion, codec, nil
}

func (c *compiledCatalog) DecodeArtifact(headerValue json.RawMessage, rows []json.RawMessage) (Artifact, error) {
	storedVersion, codec, err := c.artifactCodec(headerValue)
	if err != nil {
		return Artifact{}, err
	}
	decoded, err := codec.DecodeArtifact(headerValue, rows)
	if err != nil {
		return Artifact{}, err
	}
	return SnapshotArtifact(decoded, "format v"+itoa(storedVersion)+" decoded artifact")
}

func (c *compiledCatalog) DecodeRecoverableArtifact(headerValue json.RawMessage, rows []json.RawMessage) (Artifact, error) {
	storedVersion, codec, err := c.artifactCodec(headerValue)
	if err != nil {
		return Artifact{}, err
	}
	decoded, err := codec.DecodeRecoverableArtifact(headerValue, rows)
	if err != nil {
		return Artifact{}, err
	}
	return SnapshotArtifact(decoded, "format v"+itoa(storedVersion)+" recoverable artifact")
}

func (c *compiledCatalog) Migrate(artifact Artifact) (Artifact, error) {
	return c.chain.Migrate(artifact)
}

func (c *compiledCatalog) EncodeCurrent(artifact Artifact) (EncodedArtifact, error) {
	version, err := HeaderVersion(artifact.Header)
	if err != nil {
		return EncodedArtifact{}, err
	}
	if version != c.chain.CurrentVersion() {
		return EncodedArtifact{}, formatErrorf("encodeCurrent requires Session format v%d", c.chain.CurrentVersion())
	}
	return c.encodeCurrentArtifact(artifact)
}

func malformedResult(targetVersion, storedVersion int64, cause error) HeaderReadResult {
	return HeaderReadResult{
		Status:           HeaderMalformed,
		StoredVersion:    storedVersion,
		HasStoredVersion: true,
		TargetVersion:    targetVersion,
		Reason:           cause.Error(),
	}
}

func unsupportedReason(storedVersion, targetVersion int64) string {
	return "stored Session uses newer format v" + itoa(storedVersion) +
		"; this build writes v" + itoa(targetVersion)
}

func asUnsupported(err error, target **UnsupportedMigrationError) bool {
	return errors.As(err, target)
}

// Package sessionformatcatalog compiles the complete Session format
// migration catalog independently of mounted plugins. Port of
// packages/session/session-format-catalog (official tag
// dsh-v0.1.3-alpha.1): the direct imports make historical readability a
// build-static fact — released v0/v1 codecs and both adjacent edges are
// always present, and current restoration validates against the installed
// session package's vocabulary.
package sessionformatcatalog

import (
	"encoding/json"

	"dshgo/session"
	"dshgo/sessionformat"
	"dshgo/sessionformatv01"
	"dshgo/sessionformatv12"
)

// CurrentVersion is the installed Session format generation.
const CurrentVersion = session.SESSION_FORMAT_VERSION

// New compiles the build-static catalog: physical codec dispatch for every
// released generation, the complete adjacent migration chain, and current
// restoration through the installed session vocabulary.
func New() (sessionformat.Catalog, error) {
	return sessionformat.CompileCatalog(sessionformat.CatalogOptions{
		CurrentVersion: CurrentVersion,
		Codecs: []sessionformat.Codec{
			sessionformatv01.ReleasedV0Codec(),
			sessionformatv01.ReleasedV1Codec(),
			sessionformatv12.ReleasedV2Codec(),
		},
		Migrations: []sessionformat.Migration{
			sessionformatv01.NewMigration(),
			sessionformatv12.NewMigration(),
		},
		RestoreCurrent: func(artifact sessionformat.Artifact) (sessionformat.Artifact, error) {
			version, err := sessionformat.HeaderVersion(artifact.Header)
			if err != nil {
				return sessionformat.Artifact{}, err
			}
			if version != CurrentVersion {
				return sessionformat.Artifact{}, sessionformat.FormatErrorf(
					"installed Session format is v%d, got v%d", CurrentVersion, version)
			}
			if err := sessionformatv12.RestoreReleasedV2Artifact(artifact, session.KnownEventType); err != nil {
				return sessionformat.Artifact{}, err
			}
			return artifact, nil
		},
		RestoreCurrentHeader: func(header sessionformat.Header) (sessionformat.Header, error) {
			if err := sessionformatv12.AssertReleasedV2Header(header); err != nil {
				return nil, err
			}
			return header, nil
		},
		EncodeCurrentArtifact: func(artifact sessionformat.Artifact) (sessionformat.EncodedArtifact, error) {
			return sessionformatv12.ReleasedV2Codec().EncodeArtifact(artifact)
		},
	})
}

// encodeEventRaw is a conversion helper shared with the persistence layer.
func encodeEventRaw(event sessionformat.Event) (json.RawMessage, error) {
	return json.Marshal(event)
}

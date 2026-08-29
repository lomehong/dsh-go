// Package skillbadge ports @deepseek-ai/dsh-skill-badge: the bundled
// `dsh-badge` skill provider. The skill body ships with the package under
// assets/; the resource base points at that directory so referenced badge
// assets resolve beside the body.
package skillbadge

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"

	"dshgo/scope"
	"dshgo/skill"
)

//go:embed assets/dsh-badge.md
var assets embed.FS

// ProviderName is the unique provider name in the registry.
const ProviderName = "dsh-badge"

// SkillName is the bundled skill's name.
const SkillName = "dsh-badge"

// Description is the catalog description, verbatim from the official badge
// skill.
const Description = "Add the official “powered by dsh” badge to documents, pull requests, merge requests, and other content produced with DeepSeek Harness. Use whenever creating a pull request or merge request. Also use when the user asks for a dsh badge, powered-by-dsh attribution, or a reusable dsh badge asset or snippet."

// bodyAsset is the embedded skill body path and the extraction target name.
const bodyAsset = "assets/dsh-badge.md"

// Provider serves the single bundled dsh-badge skill from the embedded
// asset.
type Provider struct {
	// assetDir is the extracted directory holding dsh-badge.md; badge
	// resources resolve beside the body.
	assetDir string
}

// New extracts the embedded skill body into assetDir (idempotent) and builds
// the provider over it. Callers own the directory lifecycle; a session-scoped
// temp dir keeps the bundled skill per-environment.
func New(assetDir string) (*Provider, error) {
	body, err := assets.ReadFile(bodyAsset)
	if err != nil {
		return nil, fmt.Errorf("skill-badge: embedded skill body missing: %w", err)
	}
	if err := os.MkdirAll(assetDir, 0o755); err != nil {
		return nil, fmt.Errorf("skill-badge: asset dir: %w", err)
	}
	target := filepath.Join(assetDir, "dsh-badge.md")
	if err := os.WriteFile(target, body, 0o644); err != nil {
		return nil, fmt.Errorf("skill-badge: write skill body: %w", err)
	}
	return &Provider{assetDir: assetDir}, nil
}

// Name is the unique provider name.
func (p *Provider) Name() string { return ProviderName }

// List serves the one bundled candidate.
func (p *Provider) List(options skill.LookupOptions) (skill.ProviderObservation, error) {
	return skill.ProviderObservation{
		Candidates: []skill.Candidate{
			{
				Summary: skill.Summary{
					Name:         SkillName,
					Description:  Description,
					Invocation:   skill.InvocationPolicy{ModelInvocable: true, UserInvocable: true},
					Source:       "bundled",
					Provider:     ProviderName,
					ResourceBase: &skill.ResourceBase{Kind: "directory", Path: p.assetDir + string(os.PathSeparator)},
				},
				Rank:    skill.BundledSkillRank,
				Locator: filepath.Join(p.assetDir, "dsh-badge.md"),
			},
		},
		Complete: true,
	}, nil
}

// Get loads the bundled skill body from the extracted asset.
func (p *Provider) Get(candidate skill.Candidate, options skill.LookupOptions) (*skill.Definition, error) {
	body, err := os.ReadFile(filepath.Join(p.assetDir, "dsh-badge.md"))
	if err != nil {
		return nil, err
	}
	return &skill.Definition{
		Summary: skill.Summary{
			Name:         SkillName,
			Description:  Description,
			Invocation:   skill.InvocationPolicy{ModelInvocable: true, UserInvocable: true},
			Source:       "bundled",
			Provider:     ProviderName,
			ResourceBase: &skill.ResourceBase{Kind: "directory", Path: p.assetDir + string(os.PathSeparator)},
		},
		Content: string(body),
	}, nil
}

// RegisterIn registers the bundled provider on a skill registry in the given
// layer (nil = global) and returns the disposer.
func RegisterIn(registry *skill.Registry, regScope scope.ScopeKey, assetDir string) (func(), error) {
	provider, err := New(assetDir)
	if err != nil {
		return nil, err
	}
	return registry.RegisterProviderIn(regScope, func(control skill.ProviderControl) (skill.Provider, error) {
		return provider, nil
	})
}

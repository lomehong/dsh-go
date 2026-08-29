// Package skill ports @deepseek-ai/dsh-skill: the agent skill provider
// registry. This package owns the Service Definition role of the skill
// capability seam — concrete providers decide where skills come from; the
// registry merges provider catalogs, resolves the winning skill for a name,
// and exposes the winning summaries and definitions to consumers.
//
// Go adaptations: the AbortSignal becomes context.Context on LookupOptions
// and ProviderControl, and the typed Go interfaces remove the official
// runtime shape validations that exist because TypeScript values arrive
// untyped at these boundaries.
package skill

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"dshgo/scope"
)

var skillNamePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// Registry constants.
const (
	// DefaultCollectCacheEntries is the default completed-catalog cache size.
	DefaultCollectCacheEntries = 128
	// MaxCollectAttempts bounds discovery retries under concurrent revision.
	MaxCollectAttempts = 2
	// RuntimeProvider is the registry-owned provider for runtime skills.
	RuntimeProvider = "runtime"
	// RuntimeRank is the fixed candidate rank of runtime contributions.
	RuntimeRank = 250
	// BundledSkillRank is the standard precedence rank for packaged skill
	// providers and local bundled roots.
	BundledSkillRank = 600
)

// IsSkillName reports whether a string is a valid kebab-case skill name.
func IsSkillName(name string) bool {
	return skillNamePattern.MatchString(name)
}

// ResourceBase is the optional provider-specific base loaded skill bodies
// use to resolve relative resources.
type ResourceBase struct {
	// Kind: "directory", "url", or "opaque".
	Kind string `json:"kind"`
	// Path is the base directory when Kind is "directory".
	Path string `json:"path,omitempty"`
	// URL is the base URL when Kind is "url".
	URL string `json:"url,omitempty"`
	// Description is the resource description when Kind is "opaque".
	Description string `json:"description,omitempty"`
}

// InvocationPolicy carries the invocation controls shared by skill discovery
// consumers.
type InvocationPolicy struct {
	// ModelInvocable admits the skill to model-facing catalogs and loaders.
	ModelInvocable bool `json:"modelInvocable"`
	// UserInvocable admits the skill to human-facing command catalogs and
	// loaders.
	UserInvocable bool `json:"userInvocable"`
}

// Summary is the invocation-neutral skill metadata the registry's list view
// returns.
type Summary struct {
	// Name is the kebab-case identifier used to address the skill.
	Name string `json:"name"`
	// Description is the short routing description shown by discovery
	// consumers.
	Description string `json:"description"`
	// WhenToUse is the optional extra routing guidance.
	WhenToUse string `json:"whenToUse,omitempty"`
	// Invocation carries the resolved model and user invocation controls.
	Invocation InvocationPolicy `json:"invocation"`
	// Source is the discovery source that produced this winning skill
	// (prompt-visible metadata, not precedence by itself).
	Source string `json:"source"`
	// Provider owns this skill body.
	Provider string `json:"provider"`
	// ResourceBase is the optional provider-specific base for relative
	// resources.
	ResourceBase *ResourceBase `json:"resourceBase,omitempty"`
}

// Candidate is the provider catalog entry the registry merges and later
// loads through.
type Candidate struct {
	Summary
	// Rank orders duplicates within one layer; lower wins.
	Rank float64 `json:"rank"`
	// Locator is the opaque provider-owned handle passed back to Get.
	Locator any `json:"-"`
	// Path is the absolute file path when the provider has one.
	Path string `json:"path,omitempty"`
	// Metadata is the parsed optional object from provider-specific
	// frontmatter.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// Definition is the complete parsed skill, including the body loaded by
// Get.
type Definition struct {
	Summary
	// Content is the markdown instruction body after any provider-specific
	// metadata removal.
	Content string `json:"content"`
	// Path is the absolute file path when the skill came from disk.
	Path string `json:"path,omitempty"`
	// Metadata is the parsed optional frontmatter object.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// Registration is the runtime skill contribution accepted by Register;
// omitted invocation and provider fields receive defaults.
type Registration struct {
	Name         string
	Description  string
	WhenToUse    string
	Invocation   *InvocationPolicy
	Source       string
	Provider     string
	ResourceBase *ResourceBase
	Content      string
	Path         string
	Metadata     map[string]any
}

// LookupOptions is the caller context for cwd-sensitive and abortable
// provider work. Context is the Go adaptation of the official AbortSignal.
type LookupOptions struct {
	// CWD is the workspace selector for the current lookup.
	CWD string
	// Context cancels discovery or loading work for the current caller.
	Context context.Context
}

// ViewOptions adds the viewing scope; a nil scope reads the global layer
// alone.
type ViewOptions struct {
	LookupOptions
	Scope scope.ScopeKey
}

// ProviderObservation is one provider discovery outcome plus whether the
// candidates came from a complete catalog and may be cached.
type ProviderObservation struct {
	Candidates []Candidate
	Complete   bool
}

// CompleteList builds the complete-array shorthand as an observation.
func CompleteList(candidates []Candidate) ProviderObservation {
	return ProviderObservation{Candidates: candidates, Complete: true}
}

// Provider is one source of skills, such as local directories or a remote
// registry.
type Provider interface {
	// Name is the unique provider name in the registry.
	Name() string
	// List returns the available skill candidates for the current lookup
	// context. Remote initialization, authentication, and discovery belong
	// here; implementations must settle promptly when options.Context
	// aborts.
	List(options LookupOptions) (ProviderObservation, error)
	// Get loads the complete skill body for a previously listed candidate,
	// or reports absent when it is no longer loadable.
	Get(candidate Candidate, options LookupOptions) (*Definition, error)
}

// ProviderControl is the registration-scoped lifecycle and invalidation
// capability borrowed by one provider.
type ProviderControl struct {
	// Context aborts when registration fails or when the exact provider
	// registration is disposed.
	Context context.Context
	// Invalidate completed catalogs and notify consumers, only while the
	// exact registration remains active.
	Invalidate func()
}

// IsModelInvocable reports whether a skill may be advertised to and loaded
// by a model.
func IsModelInvocable(summary Summary) bool { return summary.Invocation.ModelInvocable }

// IsUserInvocable reports whether a skill may be advertised to and loaded by
// a human-facing command.
func IsUserInvocable(summary Summary) bool { return summary.Invocation.UserInvocable }

// EscapeText escapes model-facing prose embedded inside skill markup so
// provider-supplied text cannot open or close framing tags.
func EscapeText(value string) string {
	return textEscaper.Replace(value)
}

// EscapeAttr escapes one attribute value.
func EscapeAttr(value string) string {
	return attrEscaper.Replace(value)
}

var textEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
var attrEscaper = strings.NewReplacer("&", "&amp;", `"`, "&quot;", "<", "&lt;")

// RenderSkillContent renders one loaded skill for the model. The output is
// shared verbatim by the skill tool result and the user-explicit invocation
// injection, so the model sees one canonical block on both paths. The name
// rides an escaped attribute; the body is embedded verbatim (skills are
// trusted local content, and user-supplied invocation text stays outside
// this wrapper).
func RenderSkillContent(skill Definition) string {
	lines := []string{
		fmt.Sprintf(`<skill_content name="%s">`, EscapeAttr(skill.Name)),
		"<skill_resources>",
	}
	lines = append(lines, renderResourceHint(skill.Summary)...)
	lines = append(lines, "</skill_resources>", "", "<skill_instructions>", skill.Content, "</skill_instructions>", "</skill_content>")
	return strings.Join(lines, "\n")
}

func renderResourceHint(summary Summary) []string {
	base := summary.ResourceBase
	if base == nil {
		return []string{
			fmt.Sprintf(`Resources for this skill are managed by provider "%s".`, EscapeText(summary.Provider)),
			"Load referenced resources only as needed.",
		}
	}
	switch base.Kind {
	case "directory":
		return []string{
			fmt.Sprintf("Base directory for this skill: %s", EscapeText(base.Path)),
			"Resolve relative paths mentioned by this skill against the base directory before using them. Load referenced resources only as needed.",
		}
	case "url":
		return []string{
			fmt.Sprintf("Base URL for this skill: %s", EscapeText(base.URL)),
			"Resolve relative URLs mentioned by this skill against the base URL before using them. Load referenced resources only as needed.",
		}
	case "opaque":
		return []string{
			fmt.Sprintf("Resources for this skill: %s", EscapeText(base.Description)),
			"Load referenced resources only as needed.",
		}
	default:
		return []string{
			fmt.Sprintf(`Resources for this skill are managed by provider "%s".`, EscapeText(summary.Provider)),
			"Load referenced resources only as needed.",
		}
	}
}

// toSummary projects a candidate to its invocation-neutral summary.
func toSummary(candidate Candidate) Summary { return candidate.Summary }

// sortSummaries orders summaries by name code points.
func sortSummaries(summaries []Summary) {
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].Name < summaries[j].Name })
}

// validateCandidate enforces the provider catalog contract. The typed Go
// Candidate removes the official per-field typeof checks; the grammar,
// description, and provider-identity rules remain.
func validateCandidate(candidate Candidate, providerName string) error {
	if !IsSkillName(candidate.Name) {
		return fmt.Errorf(`skill provider %q returned invalid skill name %q`, providerName, candidate.Name)
	}
	if candidate.Description == "" {
		return fmt.Errorf(`skill provider %q returned skill %q without a description`, providerName, candidate.Name)
	}
	if candidate.Provider != providerName {
		return fmt.Errorf(`skill provider %q returned skill %q for provider %q`, providerName, candidate.Name, candidate.Provider)
	}
	return nil
}

// validateRuntimeSkill enforces the runtime registration contract.
func validateRuntimeSkill(skill Registration) error {
	if !IsSkillName(skill.Name) {
		return fmt.Errorf("invalid skill name %q", skill.Name)
	}
	if skill.Description == "" {
		return fmt.Errorf(`skill %q requires a description`, skill.Name)
	}
	return nil
}

// validateDefinition validates a definition loaded from a provider-controlled
// parser or remote source.
func validateDefinition(definition Definition) error {
	if !IsSkillName(definition.Name) {
		return fmt.Errorf("loaded skill has invalid name %q", definition.Name)
	}
	if definition.Description == "" {
		return fmt.Errorf(`loaded skill %q requires a description`, definition.Name)
	}
	return nil
}

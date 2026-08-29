// Package toolskill ports @deepseek-ai/dsh-tool-skill: the durable session
// skill catalog and the model-facing `skill` loader tool.
//
// Three registrations ride one plugin: the `skill` tool; the user-explicit
// `/name` gesture listener, which appends rendered <skill_content> as
// injected instructions after every other injection; and the visibility-
// matched durable catalog listener, which publishes a <system-reminder>
// catalog message only when the calling agent resolves this plugin's exact
// tool registration. The gesture listener registers first so it is the
// outermost pre-step wrapper: the catalog message enters first (background),
// the invoked skill's material lands last, closest to the model's answer.
package toolskill

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"dshgo/llm"
	"dshgo/session"
	"dshgo/skill"
)

// Name is the tool's wire name.
const Name = "skill"

// SourceKindSkillCatalog marks a durable catalog publication; FormCatalog is
// its context form.
const SourceKindSkillCatalog = "skill-catalog"

// SourceKindSkillInvocation marks a user-invoked skill load; its Form is
// FormInstructions.
const SourceKindSkillInvocation = "skill-invocation"

// PluginName is the contributing plugin's registered name.
const PluginName = "tool-skill"

// DefaultCatalogDescriptionMaxLength is the default normalized description
// length rendered in the session catalog.
const DefaultCatalogDescriptionMaxLength = 500

// gestureNameGrammar is the public skill-name grammar inside a `/name`
// gesture token. The token must be exactly `/name` bounded by whitespace or
// the text edges: a second `/` or any attached character breaks the match,
// keeping file paths (`/usr/bin`) and fractions (`5/8`) out.
var gestureNameGrammar = regexp.MustCompile(`^/([a-z0-9]+(?:-[a-z0-9]+)*)$`)

// whitespaceRuns normalizes catalog descriptions the way the official
// renderer collapses whitespace.
var whitespaceRuns = regexp.MustCompile(`\s+`)

// catalogEntry is one catalog publication entry.
type catalogEntry = llm.CatalogEntry

// catalogSourceEntries projects summaries into the durable entry list,
// mirroring the rendered catalog lines for non-model consumers.
func catalogSourceEntries(skills []skill.Summary, descriptionMaxLength int) []catalogEntry {
	entries := make([]catalogEntry, 0, len(skills))
	for _, entry := range skills {
		entries = append(entries, catalogEntry{
			Name:        entry.Name,
			Description: catalogDescription(entry.Description, descriptionMaxLength),
		})
	}
	return entries
}

// catalogDescription normalizes whitespace and ellipsizes past the bound.
func catalogDescription(value string, maxLength int) string {
	normalized := strings.TrimSpace(whitespaceRuns.ReplaceAllString(value, " "))
	runes := []rune(normalized)
	if len(runes) <= maxLength {
		return normalized
	}
	return string(runes[:maxLength-3]) + "..."
}

// renderCatalogEntries draws the model-facing catalog lines. The pseudo-XML
// escaping belongs to this frame, not to the published fact, so it is
// applied here and never stored. Names are grammar-validated and carry no
// escapable character.
func renderCatalogEntries(entries []catalogEntry) []string {
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		lines = append(lines, fmt.Sprintf("- `%s`: %s", entry.Name, skill.EscapeText(entry.Description)))
	}
	return lines
}

// digestCatalogEntries hashes the durable entry list rather than the
// rendered prose. JSON per entry rather than a separator character: every
// separator is itself a legal description character, so only quoting makes
// the boundary exact.
func digestCatalogEntries(entries []catalogEntry) string {
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		encoded, err := json.Marshal([]string{entry.Name, entry.Description})
		if err != nil {
			// Strings and a two-element array cannot fail to marshal.
			panic(fmt.Sprintf("tool-skill: catalog entry digest: %v", err))
		}
		lines = append(lines, string(encoded))
	}
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

// readCatalogEntries reads one source's durable entries, or reports absence
// when the record is not a usable catalog. A resumed, forked, or externally
// written seed only guarantees a source object with a non-empty kind; an
// unreadable record is treated as "not this plugin's catalog" rather than
// failing every subsequent turn of that session.
func readCatalogEntries(source llm.MessageSource) ([]catalogEntry, bool) {
	if len(source.CatalogEntries) == 0 {
		return nil, false
	}
	for _, entry := range source.CatalogEntries {
		if entry.Name == "" {
			return nil, false
		}
	}
	return source.CatalogEntries, true
}

// catalogHistory scans the session log backwards for this plugin's catalog
// publications: the most recent one still visible in the surface decides the
// visible digest, while any publication at all marks the catalog published.
func catalogHistory(sess *session.Session) (visibleDigest string, hasVisible bool, published bool) {
	visible := map[int64]bool{}
	for _, seq := range sess.Surface().Nodes() {
		visible[seq] = true
	}
	events := sess.Events()
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.Type != session.EventUserMessage {
			continue
		}
		message, err := session.DecodeUserMessage(event)
		if err != nil {
			continue
		}
		if message.Source.Kind != SourceKindSkillCatalog {
			continue
		}
		entries, ok := readCatalogEntries(message.Source)
		if !ok {
			continue
		}
		digest := digestCatalogEntries(entries)
		published = true
		if visible[event.Seq] {
			return digest, true, published
		}
	}
	return "", false, published
}

// stepCatalogMessage is one admitted catalog message plus its durable
// entries.
type stepCatalogMessage struct {
	message llm.Message
	entries []catalogEntry
}

// catalogMessage finds the first usable catalog message in the step's
// admitted batch.
func catalogMessage(messages []llm.Message) *stepCatalogMessage {
	for _, message := range messages {
		if message.Source.Kind != SourceKindSkillCatalog {
			continue
		}
		entries, ok := readCatalogEntries(message.Source)
		if !ok {
			continue
		}
		return &stepCatalogMessage{message: message, entries: entries}
	}
	return nil
}

// invokedSkillNames extracts `/name` gesture tokens from the claimed user
// messages, deduplicated in first-seen order. Every text block of direct
// user input is scanned; no other source can forge a gesture.
func invokedSkillNames(messages []llm.Message) []string {
	var names []string
	seen := map[string]bool{}
	for _, message := range messages {
		if message.Source.Kind != llm.SourceUser {
			continue
		}
		for _, block := range message.Content {
			if block.Type != llm.BlockText {
				continue
			}
			for _, field := range strings.Fields(block.Text) {
				match := gestureNameGrammar.FindStringSubmatch(field)
				if match == nil {
					continue
				}
				name := match[1]
				if !seen[name] {
					seen[name] = true
					names = append(names, name)
				}
			}
		}
	}
	return names
}

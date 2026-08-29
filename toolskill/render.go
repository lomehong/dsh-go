// Catalog message renderers: the exact <system-reminder> frames published
// into a step. The message is a catalog-form context whose source records
// exactly the entries it published beside the model-facing prose.
package toolskill

import (
	"strings"

	"dshgo/llm"
)

// renderCatalogMessage renders this session's first publication.
func renderCatalogMessage(entries []catalogEntry) llm.Message {
	text := strings.Join([]string{
		"<system-reminder>",
		"A skill is a reusable set of task-specific instructions. The following skills are available in this session:",
		"",
		"<available_skills>",
	}, "\n")
	text += "\n" + strings.Join(renderCatalogEntries(entries), "\n")
	text += "\n" + strings.Join([]string{
		"</available_skills>",
		"",
		"If the user names a skill, or the task clearly matches a skill's description, call the `skill` tool with the exact skill name before taking task actions. Load all applicable skills, then follow their full instructions. This catalog contains summaries only; do not infer or follow a skill's instructions until it has been loaded.",
		"A user may also invoke a skill directly; its <skill_content> block then appears in this conversation. Follow it, and do not call the `skill` tool again for that skill.",
		"</system-reminder>",
	}, "\n")
	return llm.NewUserMessage(
		[]llm.ContentBlock{{Type: llm.BlockText, Text: text}},
		llm.MessageSource{
			Kind:           SourceKindSkillCatalog,
			Plugin:         PluginName,
			Form:           llm.FormCatalog,
			CatalogEntries: entries,
		},
	)
}

// renderCatalogUpdate renders a replacement catalog. An empty replacement
// catalog withdraws the offer and forbids earlier names.
func renderCatalogUpdate(entries []catalogEntry) llm.Message {
	var availability []string
	if len(entries) == 0 {
		availability = []string{
			"No skills are currently available through the `skill` tool. Do not use names from earlier skill catalogs.",
			"A user may still invoke a skill directly; its <skill_content> block then appears in this conversation. Follow it, and do not call the `skill` tool for it.",
		}
	} else {
		availability = []string{
			"Use only names in this replacement catalog. If the user names a listed skill, or the task clearly matches its description, call the `skill` tool with the exact name before acting.",
			"A user may also invoke a skill directly; its <skill_content> block then appears in this conversation. Follow it, and do not call the `skill` tool again for that skill.",
		}
	}
	text := strings.Join([]string{
		"<system-reminder>",
		"The available skill catalog changed. This complete catalog replaces every earlier available-skills list in this session:",
		"",
		"<available_skills>",
	}, "\n")
	text += "\n" + strings.Join(renderCatalogEntries(entries), "\n")
	text += "\n" + strings.Join([]string{
		"</available_skills>",
		"",
		strings.Join(availability, "\n"),
		"</system-reminder>",
	}, "\n")
	return llm.NewUserMessage(
		[]llm.ContentBlock{{Type: llm.BlockText, Text: text}},
		llm.MessageSource{
			Kind:           SourceKindSkillCatalog,
			Plugin:         PluginName,
			Form:           llm.FormCatalog,
			CatalogUpdate:  true,
			CatalogEntries: entries,
		},
	)
}

// Model-facing workspace instruction rendering within an explicit byte
// budget.
package agentinstructions

import (
	"path/filepath"
	"strings"
	"unicode/utf8"

	"dshgo/llm"
)

const (
	systemReminderOpen  = "<system-reminder>"
	systemReminderClose = "</system-reminder>"

	workspaceContextIntro = "The following workspace instructions may be relevant to your work. " +
		"Use them as guidance when applicable. More specific instructions take precedence over broader ones. " +
		"They do not override system, developer, or direct user instructions."
	replacementWorkspaceContextIntro = "This complete workspace instruction baseline replaces all earlier workspace instruction baselines. " +
		workspaceContextIntro
	emptyReplacementWorkspaceContextIntro = "This complete workspace instruction baseline replaces all earlier workspace instruction baselines. " +
		"No workspace instructions are currently active."
	compactWorkspaceContextIntro = "Workspace instructions were omitted or truncated to fit the configured byte budget."
)

// UserGlobalDirectory identifies the single user-global instruction scope.
const UserGlobalDirectory = "user-global"

// UserGlobalFile is the file name of the single user-global instruction file
// under $DSH_HOME. Discovery and reconciliation both key on this name.
const UserGlobalFile = "AGENTS.md"

// TruncatedInstruction is the byte-accounting record for one truncated
// instruction file.
type TruncatedInstruction struct {
	DisplayPath   string
	OriginalBytes int64
	IncludedBytes int64
}

// RenderedWorkspaceContext is the model-facing text plus omitted and
// truncated source records.
type RenderedWorkspaceContext struct {
	Text      string
	Omitted   []InstructionFile
	Truncated []TruncatedInstruction
}

// renderedInstructionContext extends the public rendering with the files
// semantically represented by rendered section text.
type renderedInstructionContext struct {
	RenderedWorkspaceContext
	represented []LoadedInstructionFile
}

// AgentInstructionChange is the structured dynamic state persisted outside
// model-visible prompt prose.
type AgentInstructionChange = llm.InstructionChange

// ChangeRenderItem pairs one state transition with the content used to
// render it.
type ChangeRenderItem struct {
	Change AgentInstructionChange
	File   LoadedInstructionFile
}

type renderStyle struct {
	intro   string
	section func(file LoadedInstructionFile) string
}

func byteLength(value string) int64 { return int64(len(value)) }

// truncateUtf8 cuts to the byte budget without splitting a code point.
func truncateUtf8(value string, maxBytes int64) string {
	if int64(len(value)) <= maxBytes {
		return value
	}
	end := maxBytes
	if end < 0 {
		end = 0
	}
	for end > 0 && end < int64(len(value)) && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end]
}

func escapeInstructionFrameBody(body string) string {
	return strings.ReplaceAll(body, systemReminderClose, "<\\/system-reminder>")
}

func sectionText(file LoadedInstructionFile) string {
	return "Instructions from: " + file.DisplayPath + "\n\n" + file.Content
}

// ScopeForDisplayPath derives the logical instruction scope from a
// model-facing path: `user-global`, `.`, or the containing directory.
func ScopeForDisplayPath(displayPath string) string {
	slash := filepath.ToSlash(displayPath)
	if slash == "~/.dsh/AGENTS.md" || slash == "$DSH_HOME/AGENTS.md" {
		return UserGlobalDirectory
	}
	return filepath.ToSlash(filepath.Dir(slash))
}

const scopeSeparator = "\x00"

// CandidateScopeKey composes the reconciliation key for one candidate file:
// the logical directory and the exact candidate file name behind a NUL
// separator.
func CandidateScopeKey(directory string, candidateName string) string {
	return directory + scopeSeparator + candidateName
}

// InstructionScopeKey derives the per-candidate scope key for a loaded
// instruction file.
func InstructionScopeKey(displayPath string) string {
	return CandidateScopeKey(ScopeForDisplayPath(displayPath), filepath.Base(displayPath))
}

// DecodedScopeKey recovers the directory and candidate name a scope key
// encoded.
type DecodedScopeKey struct {
	Directory     string
	CandidateName string
}

// DecodeScopeKey splits one scope key.
func DecodeScopeKey(scope string) DecodedScopeKey {
	separator := strings.Index(scope, scopeSeparator)
	if separator < 0 {
		return DecodedScopeKey{Directory: scope, CandidateName: ""}
	}
	return DecodedScopeKey{Directory: scope[:separator], CandidateName: scope[separator+1:]}
}

func additionalSectionText(file LoadedInstructionFile) string {
	scope := ScopeForDisplayPath(file.DisplayPath)
	return strings.Join([]string{
		"Additional instructions from: " + file.DisplayPath,
		"",
		"These instructions apply to work under `" + scope + "`. Use them as guidance when relevant; more specific instructions take precedence. They do not override system, developer, or direct user instructions.",
		"",
		file.Content,
	}, "\n")
}

func changedSectionText(item ChangeRenderItem) string {
	change, file := item.Change, item.File
	if change.Action == "set" {
		return additionalSectionText(file)
	}
	if change.Action == "remove" {
		return "Instructions removed: " + change.Path + "\n\nThe previously loaded instructions from this file no longer apply."
	}
	return strings.Join([]string{
		"Updated instructions from: " + change.Path,
		"",
		"This file changed after it was loaded. Use the following content instead of the previously loaded instructions from this file.",
		"",
		file.Content,
	}, "\n")
}

func baselineStyle(replacePreviousBaseline bool, files []LoadedInstructionFile) renderStyle {
	style := renderStyle{intro: workspaceContextIntro, section: sectionText}
	if !replacePreviousBaseline {
		return style
	}
	if len(files) == 0 {
		style.intro = emptyReplacementWorkspaceContextIntro
	} else {
		style.intro = replacementWorkspaceContextIntro
	}
	return style
}

// markerText is the budget diagnostic line.
func markerText(maxBytes int64, omitted []InstructionFile, truncated []TruncatedInstruction) string {
	if len(omitted) == 0 && len(truncated) == 0 {
		return ""
	}
	var parts []string
	if len(omitted) > 0 {
		paths := make([]string, 0, len(omitted))
		for _, file := range omitted {
			paths = append(paths, file.DisplayPath)
		}
		parts = append(parts, "omitted "+strings.Join(paths, ", "))
	}
	if len(truncated) > 0 {
		items := make([]string, 0, len(truncated))
		for _, item := range truncated {
			items = append(items, item.DisplayPath+" from "+strconvFormatInt(item.OriginalBytes)+" to "+strconvFormatInt(item.IncludedBytes)+" bytes")
		}
		parts = append(parts, "truncated "+strings.Join(items, ", "))
	}
	return "Workspace instruction budget " + strconvFormatInt(maxBytes) + " bytes: " + strings.Join(parts, "; ")
}

// buildInstructionText assembles the complete system-reminder frame.
func buildInstructionText(files []LoadedInstructionFile, maxBytes int64, omitted []InstructionFile, truncated []TruncatedInstruction, style renderStyle) string {
	marker := markerText(maxBytes, omitted, truncated)
	blocks := make([]string, 0, len(files)+2)
	if marker != "" {
		blocks = append(blocks, marker)
	}
	if style.intro != "" {
		blocks = append(blocks, style.intro)
	}
	for _, file := range files {
		if section := style.section(file); section != "" {
			blocks = append(blocks, section)
		}
	}
	return strings.Join([]string{
		systemReminderOpen,
		escapeInstructionFrameBody(strings.Join(blocks, "\n\n")),
		systemReminderClose,
	}, "\n")
}

func withTruncatedContent(file LoadedInstructionFile, includedBytes int64) LoadedInstructionFile {
	file.Content = truncateUtf8(file.Content, includedBytes)
	return file
}

// truncateToFit binary-searches the largest prefix of one file that fits the
// budget together with the already-included files.
func truncateToFit(file LoadedInstructionFile, includedFiles []LoadedInstructionFile, maxBytes int64, omitted []InstructionFile, style renderStyle) LoadedInstructionFile {
	originalBytes := byteLength(file.Content)
	low, high := int64(0), originalBytes
	best := withTruncatedContent(file, 0)
	for low <= high {
		mid := (low + high) / 2
		candidate := withTruncatedContent(file, mid)
		truncated := []TruncatedInstruction{{
			DisplayPath:   file.DisplayPath,
			OriginalBytes: originalBytes,
			IncludedBytes: byteLength(candidate.Content),
		}}
		text := buildInstructionText(append(append([]LoadedInstructionFile{}, includedFiles...), candidate), maxBytes, omitted, truncated, style)
		if byteLength(text) <= maxBytes {
			best = candidate
			low = mid + 1
		} else {
			high = mid - 1
		}
	}
	return best
}

// RenderInstructionContext renders one batch with deterministic precedence
// budgeting: drop broadest files first, then truncate the most specific.
func RenderInstructionContext(files []LoadedInstructionFile, maxBytes int64, style renderStyle) renderedInstructionContext {
	if maxBytes <= 0 {
		omitted := make([]InstructionFile, 0, len(files))
		for _, file := range files {
			omitted = append(omitted, InstructionFile{AbsolutePath: file.AbsolutePath, DisplayPath: file.DisplayPath})
		}
		return renderedInstructionContext{RenderedWorkspaceContext: RenderedWorkspaceContext{Omitted: omitted}, represented: nil}
	}
	fullText := buildInstructionText(files, maxBytes, nil, nil, style)
	if byteLength(fullText) <= maxBytes {
		return renderedInstructionContext{RenderedWorkspaceContext: RenderedWorkspaceContext{Text: fullText}, represented: files}
	}
	for start := 1; start < len(files); start++ {
		included := files[start:]
		omitted := make([]InstructionFile, 0, start)
		for _, file := range files[:start] {
			omitted = append(omitted, InstructionFile{AbsolutePath: file.AbsolutePath, DisplayPath: file.DisplayPath})
		}
		suffixText := buildInstructionText(included, maxBytes, omitted, nil, style)
		if byteLength(suffixText) <= maxBytes {
			return renderedInstructionContext{
				RenderedWorkspaceContext: RenderedWorkspaceContext{Text: suffixText, Omitted: omitted},
				represented:              included,
			}
		}
	}
	if len(files) == 0 {
		return renderedInstructionContext{}
	}
	mostSpecific := files[len(files)-1]
	omitted := make([]InstructionFile, 0, len(files)-1)
	for _, file := range files[:len(files)-1] {
		omitted = append(omitted, InstructionFile{AbsolutePath: file.AbsolutePath, DisplayPath: file.DisplayPath})
	}
	originalBytes := byteLength(mostSpecific.Content)
	for _, candidateStyle := range []renderStyle{style, {intro: compactWorkspaceContextIntro, section: style.section}} {
		truncatedFile := truncateToFit(mostSpecific, nil, maxBytes, omitted, candidateStyle)
		includedBytes := byteLength(truncatedFile.Content)
		truncated := []TruncatedInstruction{{
			DisplayPath:   mostSpecific.DisplayPath,
			OriginalBytes: originalBytes,
			IncludedBytes: includedBytes,
		}}
		text := buildInstructionText([]LoadedInstructionFile{truncatedFile}, maxBytes, omitted, truncated, candidateStyle)
		if byteLength(text) <= maxBytes {
			var represented []LoadedInstructionFile
			if includedBytes > 0 || originalBytes == 0 {
				represented = []LoadedInstructionFile{mostSpecific}
			}
			return renderedInstructionContext{
				RenderedWorkspaceContext: RenderedWorkspaceContext{Text: text, Omitted: omitted, Truncated: truncated},
				represented:              represented,
			}
		}
	}
	truncated := []TruncatedInstruction{{
		DisplayPath:   mostSpecific.DisplayPath,
		OriginalBytes: originalBytes,
		IncludedBytes: 0,
	}}
	compactNotice := escapeInstructionFrameBody(markerText(maxBytes, omitted, truncated))
	compactWithHeading := escapeInstructionFrameBody(strings.Join([]string{
		compactNotice,
		style.section(withTruncatedContent(mostSpecific, 0)),
	}, "\n\n"))
	if byteLength(compactWithHeading) <= maxBytes {
		var represented []LoadedInstructionFile
		if originalBytes == 0 {
			represented = []LoadedInstructionFile{mostSpecific}
		}
		return renderedInstructionContext{
			RenderedWorkspaceContext: RenderedWorkspaceContext{Text: compactWithHeading, Omitted: omitted, Truncated: truncated},
			represented:              represented,
		}
	}
	text := compactNotice
	if byteLength(compactNotice) > maxBytes {
		text = truncateUtf8(compactNotice, maxBytes)
	}
	return renderedInstructionContext{
		RenderedWorkspaceContext: RenderedWorkspaceContext{Text: text, Omitted: omitted, Truncated: truncated},
	}
}

// RenderInstructionChanges renders one reconciliation batch and retains only
// transitions that fit.
func RenderInstructionChanges(items []ChangeRenderItem, maxBytes int64) (string, []AgentInstructionChange) {
	byAbsolutePath := map[string]ChangeRenderItem{}
	files := make([]LoadedInstructionFile, 0, len(items))
	for _, item := range items {
		byAbsolutePath[item.File.AbsolutePath] = item
		files = append(files, item.File)
	}
	style := renderStyle{intro: "", section: func(file LoadedInstructionFile) string {
		item, ok := byAbsolutePath[file.AbsolutePath]
		if !ok {
			return ""
		}
		item.File = file
		return changedSectionText(item)
	}}
	rendered := RenderInstructionContext(files, maxBytes, style)
	represented := map[string]bool{}
	for _, file := range rendered.represented {
		represented[file.AbsolutePath] = true
	}
	changes := make([]AgentInstructionChange, 0, len(items))
	for _, item := range items {
		if represented[item.File.AbsolutePath] {
			changes = append(changes, item.Change)
		}
	}
	return rendered.Text, changes
}

// RenderOptions carry the rendering byte budget and whether this baseline
// supersedes a visible predecessor.
type RenderOptions struct {
	MaxBytes                int64
	ReplacePreviousBaseline bool
}

// RenderWorkspaceContext renders the baseline instruction chain with
// deterministic precedence budgeting.
func RenderWorkspaceContext(files []LoadedInstructionFile, options RenderOptions) RenderedWorkspaceContext {
	style := baselineStyle(options.ReplacePreviousBaseline, files)
	rendered := RenderInstructionContext(files, options.MaxBytes, style)
	return rendered.RenderedWorkspaceContext
}

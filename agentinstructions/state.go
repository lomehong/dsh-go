// Session-visible workspace instruction state and dynamic reconciliation.
package agentinstructions

import (
	"path/filepath"
	"reflect"

	"dshgo/agent"
	"dshgo/llm"
	"dshgo/session"
)

func joinPath(elements ...string) string {
	return filepath.Join(elements...)
}

// Name is the plugin identity and the agent-instructions source label.
const Name = "agent-instructions"

// InstructionVersionState is the per-scope metadata cache; instruction prose
// is deliberately not retained.
type InstructionVersionState struct {
	Path          string
	Version       string
	Digest        string
	TrimmedDigest string
}

// InstructionVersionCache is session-isolated fast-path state keyed by
// logical instruction scope (host-owned map standing in for a WeakMap).
type InstructionVersionCache map[*session.Session]map[string]InstructionVersionState

// InstructionVersionUpdate is one metadata-cache transition associated with
// one rendered instruction change; State nil deletes the scope.
type InstructionVersionUpdate struct {
	Change AgentInstructionChange
	State  *InstructionVersionState
}

// ReconciledInstructionContext is the rendered reconciliation plus its
// metadata-cache transitions.
type ReconciledInstructionContext struct {
	Context        llm.Message
	VersionUpdates []InstructionVersionUpdate
}

// workspaceContextHook builds the dynamic workspace-context user message.
func workspaceContextHook(text string, changes []AgentInstructionChange) llm.Message {
	return llm.NewUserMessage([]llm.ContentBlock{{Type: "text", Text: text}}, llm.MessageSource{
		Kind:    "agent-instructions",
		Plugin:  Name,
		Form:    llm.FormInstructions,
		Changes: changes,
	})
}

// WorkspaceContextMessage builds the user-role message for a rendered
// baseline; the baseline publisher keeps only its content.
func WorkspaceContextMessage(text string) llm.Message {
	return llm.NewUserMessage([]llm.ContentBlock{{Type: "text", Text: text}}, llm.MessageSource{
		Kind:   llm.SourcePlugin,
		Plugin: Name,
	})
}

func isWorkspaceContextSource(source llm.MessageSource) bool {
	return source.Kind == "agent-instructions"
}

// IsWorkspaceContext reports whether one user message carries the
// workspace-instructions context.
func IsWorkspaceContext(message llm.Message) bool {
	return message.Source.Kind == "agent-instructions"
}

// SameContextPayload compares content and source deeply.
func SameContextPayload(left llm.Message, right llm.Message) bool {
	return sameContentBlocks(left.Content, right.Content) && sourcesEqual(left.Source, right.Source)
}

func sameContentBlocks(left []llm.ContentBlock, right []llm.ContentBlock) bool {
	return reflect.DeepEqual(left, right)
}

func sourcesEqual(left llm.MessageSource, right llm.MessageSource) bool {
	return reflect.DeepEqual(left, right)
}

// VisibleBaselineSource recovers the baseline source of the most recent
// visible baseline message, claimed messages first, then the session surface.
func VisibleBaselineSource(a *agent.Agent, authorityMessages []llm.Message) *llm.MessageSource {
	for i := len(authorityMessages) - 1; i >= 0; i-- {
		source := authorityMessages[i].Source
		if source.Kind == "agent-instructions" && source.Baseline {
			copied := source
			return &copied
		}
	}
	events := a.Session.Events()
	for i := len(a.Session.Surface().Nodes()) - 1; i >= 0; i-- {
		seq := a.Session.Surface().Nodes()[i]
		if seq < 0 || seq >= int64(len(events)) {
			continue
		}
		event := events[seq]
		if event.Type != session.EventUserMessage {
			continue
		}
		decoded, err := session.DecodeUserMessage(event)
		if err != nil || decoded.Source.Kind != "agent-instructions" || !decoded.Source.Baseline {
			continue
		}
		copied := decoded.Source
		return &copied
	}
	return nil
}

// visibleInstructionChanges folds every visible workspace-context change by
// scope, later events and claimed messages winning.
func visibleInstructionChanges(a *agent.Agent, authorityMessages []llm.Message) map[string]AgentInstructionChange {
	events := a.Session.Events()
	visibleSeqs := map[int64]bool{}
	for _, seq := range a.Session.Surface().Nodes() {
		visibleSeqs[seq] = true
	}
	visible := map[string]AgentInstructionChange{}
	for _, event := range events {
		if event.Type != session.EventUserMessage {
			continue
		}
		decoded, err := session.DecodeUserMessage(event)
		if err != nil || !isWorkspaceContextSource(decoded.Source) {
			continue
		}
		for _, change := range decoded.Source.Changes {
			if visibleSeqs[event.Seq] {
				visible[change.Scope] = change
			}
		}
	}
	for _, message := range authorityMessages {
		if !isWorkspaceContextSource(message.Source) {
			continue
		}
		for _, change := range message.Source.Changes {
			visible[change.Scope] = change
		}
	}
	return visible
}

// BaselineInstructionState converts retained baseline files into comparison
// and metadata-cache state.
func BaselineInstructionState(files []LoadedInstructionFile) (changes map[string]AgentInstructionChange, versions map[string]InstructionVersionState) {
	changes = map[string]AgentInstructionChange{}
	versions = map[string]InstructionVersionState{}
	for _, file := range files {
		digest := InstructionContentSha1(file.Content)
		change := AgentInstructionChange{
			Action: "set",
			Scope:  InstructionScopeKey(file.DisplayPath),
			Path:   file.DisplayPath,
			Digest: digest,
		}
		changes[change.Scope] = change
		if file.Version != "" {
			versions[change.Scope] = InstructionVersionState{
				Path:          file.DisplayPath,
				Version:       file.Version,
				Digest:        digest,
				TrimmedDigest: TrimmedInstructionDigest(file.Content),
			}
		}
	}
	return changes, versions
}

func versionStatesFor(cache InstructionVersionCache, session *session.Session) map[string]InstructionVersionState {
	states := cache[session]
	if states == nil {
		states = map[string]InstructionVersionState{}
		cache[session] = states
	}
	return states
}

func sameInstructionChange(a AgentInstructionChange, b AgentInstructionChange) bool {
	return a.Action == b.Action && a.Scope == b.Scope && a.Path == b.Path && a.Digest == b.Digest
}

// RetainedInstructionVersionUpdates keeps only cache updates represented by
// rendered changes.
func RetainedInstructionVersionUpdates(updates []InstructionVersionUpdate, renderedChanges []AgentInstructionChange) []InstructionVersionUpdate {
	retained := make([]InstructionVersionUpdate, 0, len(updates))
	for _, update := range updates {
		for _, change := range renderedChanges {
			if sameInstructionChange(update.Change, change) {
				retained = append(retained, update)
				break
			}
		}
	}
	return retained
}

// ApplyInstructionVersionUpdates applies metadata-cache transitions without
// retaining instruction prose.
func ApplyInstructionVersionUpdates(session *session.Session, updates []InstructionVersionUpdate, cache InstructionVersionCache) {
	if len(updates) == 0 {
		return
	}
	states := versionStatesFor(cache, session)
	for _, update := range updates {
		if update.State == nil {
			delete(states, update.Change.Scope)
		} else {
			states[update.Change.Scope] = *update.State
		}
	}
	if len(states) == 0 {
		delete(cache, session)
	}
}

func relativeScope(projectRoot string, dir string) string {
	scope := RelativeDisplay(projectRoot, dir)
	if scope == "" {
		return "."
	}
	return scope
}

// ScopeKeyDirectory resolves one scope's directory to a filesystem directory.
func scopeDirectory(scope string, projectRoot string, dshHome string) string {
	decoded := DecodeScopeKey(scope)
	switch decoded.Directory {
	case UserGlobalDirectory:
		return dshHome
	case ".":
		return projectRoot
	default:
		return joinPath(projectRoot, decoded.Directory)
	}
}

// ProbeScopeInstruction probes the current host metadata for one
// per-candidate instruction scope.
func ProbeScopeInstruction(scope string, projectRoot string, resolved ResolvedConfig) (probedInstructionFile, probeKind) {
	decoded := DecodeScopeKey(scope)
	dir := scopeDirectory(scope, projectRoot, resolved.DSHHome)
	absolutePath := joinPath(dir, decoded.CandidateName)
	probe := statFile(absolutePath)
	if probe.kind != probePresent {
		return probedInstructionFile{}, probe.kind
	}
	displayPath := RelativeDisplay(projectRoot, absolutePath)
	if decoded.Directory == UserGlobalDirectory {
		displayPath = userGlobalDisplayPath(resolved.DSHHome)
	}
	return probedInstructionFile{
		instructionFile: instructionFile{absolutePath: absolutePath, displayPath: displayPath},
		version:         probe.version,
		size:            probe.size,
	}, probePresent
}

// ReconcileOptions carry the authoritative claimed context, pending scope
// hints, touched paths, and baseline participation.
type ReconcileOptions struct {
	AuthorityMessages      []llm.Message
	ScopeMessages          []llm.Message
	TouchedPaths           []string
	IncludeBaselineScopes  bool
	ExcludedBaselineScopes map[string]bool
	ProjectRoot            string
}

// ReconcileInstructionContext compares visible state with host-visible files
// and renders transitions. Nil means unchanged or nothing renderable.
func ReconcileInstructionContext(a *agent.Agent, resolved ResolvedConfig, versionCache InstructionVersionCache, options ReconcileOptions) *ReconciledInstructionContext {
	sess := a.Session
	effective := visibleInstructionChanges(a, options.AuthorityMessages)
	effectiveOrder := visibleInstructionChangeOrder(a, options.AuthorityMessages)
	cwd := sess.Header().CWD
	projectRoot := options.ProjectRoot
	if projectRoot == "" {
		projectRoot = FindProjectRoot(cwd, resolved.ProjectRootMarkers)
	}
	// Ordered scope set: insertion order is model-visible (render order),
	// so it is tracked explicitly instead of relying on map iteration.
	var orderedScopes []string
	scopes := map[string]bool{}
	addScope := func(scope string) {
		if !scopes[scope] {
			scopes[scope] = true
			orderedScopes = append(orderedScopes, scope)
		}
	}
	baselineScopes := map[string]bool{}
	var orderedBaselineScopes []string
	addBaselineScope := func(scope string) {
		if !baselineScopes[scope] {
			baselineScopes[scope] = true
			orderedBaselineScopes = append(orderedBaselineScopes, scope)
		}
	}
	addDirScopes := func(add func(string), directory string) {
		for _, candidate := range resolved.InstructionFileCandidates {
			add(CandidateScopeKey(directory, candidate))
		}
		for _, candidate := range resolved.LocalInstructionFileCandidates {
			add(CandidateScopeKey(directory, candidate))
		}
	}
	addProjectScopes := func(add func(string), dir string) {
		addDirScopes(add, relativeScope(projectRoot, dir))
	}
	addBaselineScope(CandidateScopeKey(UserGlobalDirectory, UserGlobalFile))
	for _, dir := range AncestorChain(projectRoot, cwd) {
		addProjectScopes(addBaselineScope, dir)
	}
	if options.IncludeBaselineScopes {
		for _, scope := range orderedBaselineScopes {
			addScope(scope)
		}
	}
	for _, message := range options.ScopeMessages {
		if !isWorkspaceContextSource(message.Source) {
			continue
		}
		for _, change := range message.Source.Changes {
			if !options.IncludeBaselineScopes && baselineScopes[change.Scope] {
				continue
			}
			addScope(change.Scope)
		}
	}
	for _, scope := range effectiveOrder {
		if !options.IncludeBaselineScopes && baselineScopes[scope] {
			continue
		}
		decoded := DecodeScopeKey(scope)
		if decoded.Directory == UserGlobalDirectory {
			addScope(CandidateScopeKey(UserGlobalDirectory, UserGlobalFile))
		} else {
			addDirScopes(addScope, decoded.Directory)
		}
	}
	for _, touchedPath := range options.TouchedPaths {
		for _, dir := range DescendantDirsBetween(cwd, touchedPath) {
			addProjectScopes(addScope, dir)
		}
	}

	versions := versionStatesFor(versionCache, sess)
	seenAbsolutePaths := map[string]bool{}
	keptTrimmedByDir := map[string]map[string]bool{}
	registerKeptTrimmed := func(directory string, digest string) bool {
		digests := keptTrimmedByDir[directory]
		if digests == nil {
			digests = map[string]bool{}
			keptTrimmedByDir[directory] = digests
		}
		if digests[digest] {
			return true
		}
		digests[digest] = true
		return false
	}
	var items []ChangeRenderItem
	var versionUpdates []InstructionVersionUpdate
	pushRemoval := func(scope string, path string) {
		change := AgentInstructionChange{Action: "remove", Scope: scope, Path: path}
		items = append(items, ChangeRenderItem{
			Change: change,
			File:   LoadedInstructionFile{AbsolutePath: "removed:" + scope, DisplayPath: path},
		})
		versionUpdates = append(versionUpdates, InstructionVersionUpdate{Change: change})
	}

	// Group scopes by directory, preserving first-seen order of both levels.
	var directories []string
	directoriesSeen := map[string]bool{}
	scopesByDirectory := map[string][]string{}
	for _, scope := range orderedScopes {
		directory := DecodeScopeKey(scope).Directory
		if !directoriesSeen[directory] {
			directoriesSeen[directory] = true
			directories = append(directories, directory)
		}
		scopesByDirectory[directory] = append(scopesByDirectory[directory], scope)
	}
	for _, directory := range directories {
		directoryScopes := scopesByDirectory[directory]
		var probedScopes []string
		for _, scope := range directoryScopes {
			if options.ExcludedBaselineScopes != nil && baselineScopes[scope] && options.ExcludedBaselineScopes[scope] {
				previous, ok := effective[scope]
				if !ok || previous.Action == "remove" {
					delete(versions, scope)
				} else {
					pushRemoval(scope, previous.Path)
				}
			} else {
				probedScopes = append(probedScopes, scope)
			}
		}
		itemStart := len(items)
		versionUpdateStart := len(versionUpdates)
		var addedAbsolutePaths []string
		priorVersions := map[string]*InstructionVersionState{}
		for _, scope := range probedScopes {
			if state, ok := versions[scope]; ok {
				copied := state
				priorVersions[scope] = &copied
			}
		}
		for _, scope := range probedScopes {
			previous, hasPrevious := effective[scope]
			probedFile, probeKind := ProbeScopeInstruction(scope, projectRoot, resolved)
			if probeKind == probeUnavailable {
				if !hasPrevious || previous.Action == "remove" {
					continue
				}
				// Same-directory candidates form one deduplicated authority
				// group: preserve the entire last-good group when an active
				// member cannot be observed.
				items = items[:itemStart]
				versionUpdates = versionUpdates[:versionUpdateStart]
				for candidateScope, prior := range priorVersions {
					if prior == nil {
						delete(versions, candidateScope)
					} else {
						versions[candidateScope] = *prior
					}
				}
				for _, absolutePath := range addedAbsolutePaths {
					delete(seenAbsolutePaths, absolutePath)
				}
				delete(keptTrimmedByDir, directory)
				break
			}
			if probeKind == probeAbsent {
				if !hasPrevious || previous.Action == "remove" {
					delete(versions, scope)
				} else {
					pushRemoval(scope, previous.Path)
				}
				continue
			}
			if seenAbsolutePaths[probedFile.absolutePath] {
				continue
			}
			seenAbsolutePaths[probedFile.absolutePath] = true
			addedAbsolutePaths = append(addedAbsolutePaths, probedFile.absolutePath)
			cached, hasCached := versions[scope]
			if hasCached &&
				cached.Path == probedFile.displayPath &&
				cached.Version == probedFile.version &&
				hasPrevious &&
				previous.Action != "remove" &&
				previous.Path == cached.Path &&
				previous.Digest == cached.Digest {
				// Unchanged and previously rendered: keep it, but an earlier
				// sibling that now matches its trimmed content makes this the
				// duplicate to remove.
				if registerKeptTrimmed(directory, cached.TrimmedDigest) {
					pushRemoval(scope, previous.Path)
				}
				continue
			}
			file := ReadScopeInstruction(probedFile, resolved.MaxSourceBytes)
			if file == nil {
				continue
			}
			currentDigest := InstructionContentSha1(file.Content)
			trimmedDigest := TrimmedInstructionDigest(file.Content)
			if registerKeptTrimmed(directory, trimmedDigest) {
				// A distinct file whose trimmed content already appeared
				// earlier in this directory: drop it, removing any copy that
				// was previously rendered.
				if hasPrevious && previous.Action != "remove" {
					pushRemoval(scope, previous.Path)
				} else {
					delete(versions, scope)
				}
				continue
			}
			nextVersion := InstructionVersionState{
				Path:          file.DisplayPath,
				Version:       probedFile.version,
				Digest:        currentDigest,
				TrimmedDigest: trimmedDigest,
			}
			if hasPrevious && previous.Action != "remove" && previous.Path == file.DisplayPath && previous.Digest == currentDigest {
				versions[scope] = nextVersion
			} else {
				action := "replace"
				if !hasPrevious || previous.Action == "remove" {
					action = "set"
				}
				change := AgentInstructionChange{
					Action: action,
					Scope:  scope,
					Path:   file.DisplayPath,
					Digest: currentDigest,
				}
				items = append(items, ChangeRenderItem{Change: change, File: *file})
				copied := nextVersion
				versionUpdates = append(versionUpdates, InstructionVersionUpdate{Change: change, State: &copied})
			}
		}
	}
	if len(items) == 0 {
		return nil
	}
	text, renderedChanges := RenderInstructionChanges(items, resolved.MaxBytes)
	// When no transition survived rendering (tiny budgets render notice-only
	// text), emit nothing and commit nothing — the uncommitted versions make
	// the next pass retry instead of spamming notice-only contexts.
	if text == "" || len(renderedChanges) == 0 {
		return nil
	}
	return &ReconciledInstructionContext{
		Context:        workspaceContextHook(text, renderedChanges),
		VersionUpdates: RetainedInstructionVersionUpdates(versionUpdates, renderedChanges),
	}
}

// visibleInstructionChangeOrder returns the scope keys of the visible state
// in first-seen order: events by seq, then authority messages.
func visibleInstructionChangeOrder(a *agent.Agent, authorityMessages []llm.Message) []string {
	var order []string
	seen := map[string]bool{}
	add := func(change AgentInstructionChange) {
		if !seen[change.Scope] {
			seen[change.Scope] = true
			order = append(order, change.Scope)
		}
	}
	events := a.Session.Events()
	visibleSeqs := map[int64]bool{}
	for _, seq := range a.Session.Surface().Nodes() {
		visibleSeqs[seq] = true
	}
	for _, event := range events {
		if event.Type != session.EventUserMessage || !visibleSeqs[event.Seq] {
			continue
		}
		decoded, err := session.DecodeUserMessage(event)
		if err != nil || !isWorkspaceContextSource(decoded.Source) {
			continue
		}
		for _, change := range decoded.Source.Changes {
			add(change)
		}
	}
	for _, message := range authorityMessages {
		if !isWorkspaceContextSource(message.Source) {
			continue
		}
		for _, change := range message.Source.Changes {
			add(change)
		}
	}
	return order
}

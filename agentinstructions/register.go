// Plugin wiring: baseline composition, inbox synchronization, and
// projection of tool touches.
package agentinstructions

import (
	"context"
	"fmt"
	"strings"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/tools"
)

func trimSpace(value string) string { return strings.TrimSpace(value) }

// fileTouchToolNames are the tools whose successful execution counts as an
// instruction touch.
var fileTouchToolNames = map[string]bool{"read": true, "write": true, "edit": true}

// projectionTouch is one successful tool execution's file path plus the
// owning agent.
type projectionTouch struct {
	agent *agent.Agent
	path  string
}

// baselinePreparation is the per-session reuse record: the identity whose
// reload produced the retained omitted-scope set.
type baselinePreparation struct {
	identity       string
	excludedScopes map[string]bool
}

// Register mounts the workspace instruction loader: baseline instructions
// enter durable context before the first request; successful read/write/edit
// tool touches project nested, changed, and removed instructions into the
// inbox for later steps. The returned disposer detaches every listener.
//
// Go adaptations: there is no ctx.fs service (host filesystem reads are
// unconditional), and the async projection tail is synchronous — touches
// project inline from the tools/result listener, which only the next
// pre-step observes, so the official step-open deferral is unnecessary.
func Register(agents *agent.AgentRegistry, runtime *tools.ToolRuntime, logger cordis.Logger, config Config) (func(), error) {
	if agents == nil {
		return nil, fmt.Errorf("agent-instructions: agents registry is required")
	}
	if runtime == nil {
		return nil, fmt.Errorf("agent-instructions: tool runtime is required")
	}
	if logger == nil {
		logger = cordis.Discard{}
	}
	resolved := ResolveConfig(config)
	instructionVersions := InstructionVersionCache{}
	baselinePreparations := map[*session.Session]baselinePreparation{}
	executionTouches := map[*tools.ExecutionToken][]projectionTouch{}

	compose := func(a *agent.Agent, signal context.Context, claimed []llm.Message, pending []llm.Message, touchedPaths []string) *llm.Message {
		if signal != nil && signal.Err() != nil {
			return nil
		}
		if resolved.MaxBytes <= 0 {
			return nil
		}
		if len(touchedPaths) == 0 && len(pending) > 0 {
			first := pending[0]
			return &first
		}
		var content []llm.ContentBlock
		var changes []AgentInstructionChange
		desiredBaseline := false
		authorityMessages := append([]llm.Message{}, claimed...)
		sess := a.Session
		cwd := sess.Header().CWD
		projectRoot := FindProjectRoot(cwd, resolved.ProjectRootMarkers)
		identity := WorkspaceBaselineIdentity(resolved, cwd, projectRoot)
		visibleBaseline := VisibleBaselineSource(a, authorityMessages)
		baselinePresent := visibleBaseline != nil
		keepVisibleBaseline := visibleBaseline != nil && visibleBaseline.BaselineIdentity == identity
		prepared, hasPrepared := baselinePreparations[sess]
		excludedBaselineScopes := map[string]bool{}
		haveExcludedBaselineScopes := keepVisibleBaseline && hasPrepared && prepared.identity == identity
		if haveExcludedBaselineScopes {
			excludedBaselineScopes = prepared.excludedScopes
		}
		nextPreparation := (*baselinePreparation)(nil)
		if !baselinePresent || !keepVisibleBaseline || !haveExcludedBaselineScopes {
			replacePreviousBaseline := baselinePresent && !keepVisibleBaseline
			instructions := LoadBaselineInstructionSet(resolved, discoverOptions{
				cwd:         cwd,
				projectRoot: projectRoot,
			}, replacePreviousBaseline)
			var includedChanges map[string]AgentInstructionChange
			var includedVersions map[string]InstructionVersionState
			var observedChanges map[string]AgentInstructionChange
			if instructions != nil {
				includedChanges, includedVersions = BaselineInstructionState(instructions.Included)
				observedChanges, _ = BaselineInstructionState(instructions.Observed)
			} else {
				includedChanges, includedVersions = BaselineInstructionState(nil)
				observedChanges, _ = BaselineInstructionState(nil)
			}
			excludedScopes := map[string]bool{}
			for scope := range observedChanges {
				excludedScopes[scope] = true
			}
			for scope := range includedChanges {
				delete(excludedScopes, scope)
			}
			excludedBaselineScopes = excludedScopes
			haveExcludedBaselineScopes = true
			nextPreparation = &baselinePreparation{identity: identity, excludedScopes: excludedScopes}
			if len(includedVersions) > 0 {
				versionStates := versionStatesFor(instructionVersions, sess)
				for scope, state := range includedVersions {
					versionStates[scope] = state
				}
			}
			if !keepVisibleBaseline && instructions != nil && instructions.Rendered.Text != "" {
				baselineContent := WorkspaceContextMessage(instructions.Rendered.Text).Content
				content = append(content, baselineContent...)
				replacementScopes := map[string]bool{}
				for scope := range includedChanges {
					replacementScopes[scope] = true
				}
				var baselineChangesList []AgentInstructionChange
				if replacePreviousBaseline {
					for _, change := range visibleBaseline.Changes {
						if change.Action == "remove" || replacementScopes[change.Scope] {
							continue
						}
						baselineChangesList = append(baselineChangesList, AgentInstructionChange{
							Action: "remove",
							Scope:  change.Scope,
							Path:   change.Path,
						})
					}
				}
				// Deterministic map order: the baseline state keys follow the
				// retained-file order, so sort by that order's insertion —
				// recover it from the rendered file list.
				for _, file := range instructions.Included {
					scope := InstructionScopeKey(file.DisplayPath)
					if change, ok := includedChanges[scope]; ok {
						baselineChangesList = append(baselineChangesList, change)
					}
				}
				changes = append(changes, baselineChangesList...)
				authority := llm.NewUserMessage(append([]llm.ContentBlock{}, baselineContent...), llm.MessageSource{
					Kind:             "agent-instructions",
					Plugin:           Name,
					Form:             llm.FormInstructions,
					Baseline:         true,
					BaselineIdentity: identity,
					Changes:          baselineChangesList,
				})
				authorityMessages = append(authorityMessages, authority)
				desiredBaseline = true
			}
		}
		var excludedFilter map[string]bool
		if haveExcludedBaselineScopes {
			excludedFilter = excludedBaselineScopes
		}
		update := ReconcileInstructionContext(a, resolved, instructionVersions, ReconcileOptions{
			AuthorityMessages:      authorityMessages,
			ScopeMessages:          pending,
			IncludeBaselineScopes:  keepVisibleBaseline,
			ExcludedBaselineScopes: excludedFilter,
			TouchedPaths:           touchedPaths,
			ProjectRoot:            projectRoot,
		})
		if update != nil {
			content = append(content, update.Context.Content...)
			changes = append(changes, update.Context.Source.Changes...)
			ApplyInstructionVersionUpdates(sess, update.VersionUpdates, instructionVersions)
		}
		if nextPreparation != nil {
			baselinePreparations[sess] = *nextPreparation
		}
		if len(content) == 0 {
			return nil
		}
		message := llm.NewUserMessage(content, llm.MessageSource{
			Kind:             "agent-instructions",
			Plugin:           Name,
			Form:             llm.FormInstructions,
			Baseline:         desiredBaseline,
			BaselineIdentity: identity,
			Changes:          changes,
		})
		return &message
	}

	syncInbox := func(a *agent.Agent, claimed []llm.Message, desired *llm.Message) {
		inbox := a.Inbox
		var pending []llm.Message
		for _, message := range inbox.NextStep() {
			if IsWorkspaceContext(message) {
				pending = append(pending, message)
			}
		}
		removeAll := func() {
			for _, message := range pending {
				_, _ = inbox.Remove(message.ID)
			}
		}
		alreadySupplied := false
		if desired != nil {
			for _, message := range claimed {
				if SameContextPayload(message, *desired) {
					alreadySupplied = true
					break
				}
			}
			if !alreadySupplied {
				events := a.Session.Events()
				for _, seq := range a.Session.Surface().Nodes() {
					if seq < 0 || seq >= int64(len(events)) {
						continue
					}
					event := events[seq]
					if event.Type != session.EventUserMessage {
						continue
					}
					decoded, err := session.DecodeUserMessage(event)
					if err == nil && SameContextPayload(decoded, *desired) {
						alreadySupplied = true
						break
					}
				}
			}
		}
		if desired == nil || alreadySupplied {
			removeAll()
			return
		}
		reusableIndex := -1
		for i, message := range pending {
			if SameContextPayload(message, *desired) {
				reusableIndex = i
				break
			}
		}
		if reusableIndex >= 0 {
			for i, message := range pending {
				if i != reusableIndex {
					_, _ = inbox.Remove(message.ID)
				}
			}
			return
		}
		if len(pending) == 0 {
			_ = inbox.Prepend(agent.InboxNextStep, *desired)
		} else {
			_, _ = inbox.Replace(pending[0].ID, *desired)
		}
		for i := 1; i < len(pending); i++ {
			_, _ = inbox.Remove(pending[i].ID)
		}
	}

	composeAndSync := func(a *agent.Agent, claimed []llm.Message, touchedPaths []string) {
		var pending []llm.Message
		for _, message := range a.Inbox.NextStep() {
			if IsWorkspaceContext(message) {
				pending = append(pending, message)
			}
		}
		desired := compose(a, nil, claimed, pending, touchedPaths)
		syncInbox(a, claimed, desired)
	}

	// resolveAgent maps a tool execution's scope back to the live agent.
	resolveAgent := func(scope tools.ScopeKey) *agent.Agent {
		return agents.ByScope(scope)
	}

	preStepDetach := agents.Events().PreStep().On(nil, func(stepPayload agent.PreStepPayload, next func(agent.PreStepPayload) agent.PreStepDecision) agent.PreStepDecision {
		decision := next(stepPayload)
		a := stepPayload.Agent
		// Touches that arrived during an open step settle before this
		// composition, mirroring the official step/end flush: the inline
		// projection already ran, so nothing queues here.
		pending := workspacePending(a.Inbox)
		desired := compose(a, stepPayload.Signal, stepPayload.Messages, pending, nil)
		if decision.Kind != "enter" || (stepPayload.Step == 1 && len(decision.Messages) == 0) {
			// An empty first entry owns a no-step turn: keep context pending
			// instead of turning it into a standalone request.
			syncInbox(a, stepPayload.Messages, desired)
			return decision
		}
		// A proceeding step settles the pending context: it either enters
		// below as `desired`, or its payload is already covered by the batch.
		for _, message := range pending {
			_, _ = a.Inbox.Remove(message.ID)
		}
		if desired == nil || containsSameContext(decision.Messages, *desired) {
			return decision
		}
		// Fold the context right after the claimed batch, so the direct
		// prompt precedes it and the driver-appended runtime context follows.
		lastClaimedIndex := -1
		for i, message := range decision.Messages {
			if containsID(stepPayload.Messages, message.ID) {
				lastClaimedIndex = i
			}
		}
		entered := make([]llm.Message, 0, len(decision.Messages)+1)
		entered = append(entered, decision.Messages[:lastClaimedIndex+1]...)
		entered = append(entered, *desired)
		entered = append(entered, decision.Messages[lastClaimedIndex+1:]...)
		decision.Messages = entered
		return decision
	})

	resultDetach := runtime.OnResult(nil, func(exec *tools.ToolExecution, result *tools.ToolExecutionResult) {
		touches := executionTouches[exec.Token]
		delete(executionTouches, exec.Token)
		if !result.IsError && exec.Agent != nil {
			if path, ok := filePathFromExecution(exec); ok {
				if a := resolveAgent(exec.Agent); a != nil {
					touches = append(touches, projectionTouch{agent: a, path: path})
				}
			}
		}
		if exec.Parent != nil {
			if len(touches) > 0 {
				executionTouches[exec.Parent] = append(executionTouches[exec.Parent], touches...)
			}
			return
		}
		for _, touch := range touches {
			composeAndSync(touch.agent, nil, []string{touch.path})
		}
	})

	return func() {
		resultDetach()
		preStepDetach()
	}, nil
}

// workspacePending reads the next-step workspace-context entries.
func workspacePending(inbox *agent.Inbox) []llm.Message {
	var pending []llm.Message
	for _, message := range inbox.NextStep() {
		if IsWorkspaceContext(message) {
			pending = append(pending, message)
		}
	}
	return pending
}

// filePathFromExecution extracts the touched file path from a read, write,
// or edit execution.
func filePathFromExecution(exec *tools.ToolExecution) (string, bool) {
	if !fileTouchToolNames[exec.Name] {
		return "", false
	}
	arguments, ok := exec.Arguments.(map[string]any)
	if !ok {
		return "", false
	}
	raw, ok := arguments["file_path"]
	if !ok {
		return "", false
	}
	filePath, ok := raw.(string)
	if !ok {
		return "", false
	}
	trimmed := trimSpace(filePath)
	return trimmed, len(trimmed) > 0
}

func containsSameContext(messages []llm.Message, desired llm.Message) bool {
	for _, message := range messages {
		if SameContextPayload(message, desired) {
			return true
		}
	}
	return false
}

func containsID(messages []llm.Message, id llm.MessageID) bool {
	for _, message := range messages {
		if message.ID == id {
			return true
		}
	}
	return false
}

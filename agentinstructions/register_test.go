package agentinstructions

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dshgo/agent"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/tools"
)

// testFixture isolates one project tree, harness home, and live agent.
type testFixture struct {
	t        *testing.T
	root     string
	dshHome  string
	project  string
	nested   string
	registry *agent.AgentRegistry
	runtime  *tools.ToolRuntime
	agent    *agent.Agent
	sess     *session.Session
	undo     func()
}

func newFixture(t *testing.T) *testFixture {
	t.Helper()
	root := t.TempDir()
	f := &testFixture{
		t:       t,
		root:    root,
		dshHome: filepath.Join(root, "dsh-home"),
		project: filepath.Join(root, "project"),
		nested:  filepath.Join(root, "project", "nested"),
	}
	if err := os.MkdirAll(filepath.Join(f.project, ".git"), 0o755); err != nil {
		t.Fatalf("project marker: %v", err)
	}
	if err := os.MkdirAll(f.nested, 0o755); err != nil {
		t.Fatalf("nested: %v", err)
	}
	if err := os.MkdirAll(f.dshHome, 0o755); err != nil {
		t.Fatalf("dsh home: %v", err)
	}
	f.writeRoot("root instructions\n")
	f.writeNested("nested instructions\n")
	f.writeUserGlobal("user-global instructions\n")
	f.startPlugin(Config{MaxBytes: 1 << 20})
	f.startAgent()
	t.Cleanup(f.undo)
	return f
}

func (f *testFixture) writeRoot(content string) {
	f.t.Helper()
	f.writeFile(filepath.Join(f.project, "AGENTS.md"), content)
}

func (f *testFixture) writeNested(content string) {
	f.t.Helper()
	f.writeFile(filepath.Join(f.nested, "AGENTS.md"), content)
}

func (f *testFixture) writeUserGlobal(content string) {
	f.t.Helper()
	f.writeFile(filepath.Join(f.dshHome, "AGENTS.md"), content)
}

func (f *testFixture) writeFile(path string, content string) {
	f.t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		f.t.Fatalf("write %s: %v", path, err)
	}
}

// startPlugin mounts one plugin instance with the given config.
func (f *testFixture) startPlugin(config Config) {
	f.t.Helper()
	if f.undo != nil {
		f.undo()
	}
	registry := agent.NewAgentRegistry(nil, nil)
	runtime, err := tools.NewToolRuntime(nil, tools.Config{})
	if err != nil {
		f.t.Fatalf("runtime: %v", err)
	}
	config.DSHHome = f.dshHome
	undo, err := Register(registry, runtime, nil, config)
	if err != nil {
		f.t.Fatalf("register: %v", err)
	}
	f.registry = registry
	f.runtime = runtime
	f.undo = undo
}

func (f *testFixture) startAgent() {
	f.t.Helper()
	sess, err := session.NewDetached(session.SessionID("agent-1"), nil, &session.SessionHeader{ID: session.SessionID("agent-1"), CWD: f.nested}, 0)
	if err != nil {
		f.t.Fatalf("session: %v", err)
	}
	inbox, err := agent.NewInbox(sess, nopNotifications{})
	if err != nil {
		f.t.Fatalf("inbox: %v", err)
	}
	built := agent.NewAgent(agent.AgentConfig{ID: sess.ID(), Session: sess, Inbox: inbox}, f.registry.Events())
	if _, err := f.registry.Enter(built, nil); err != nil {
		f.t.Fatalf("enter: %v", err)
	}
	f.agent = built
	f.sess = sess
}

var openSchema = true

// nopNotifications satisfies the inbox live-notification contract in tests.
type nopNotifications struct{}

func (nopNotifications) Inserted(llm.Message)       {}
func (nopNotifications) Discarded(llm.Message)      {}
func (nopNotifications) Claimed(llm.Message, int64) {}

func userMessage(text string) llm.Message {
	return llm.NewUserMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: text}}, llm.MessageSource{Kind: llm.SourceUser})
}

// publishBaseline appends a user message to the visible surface.
func (f *testFixture) publish(msg llm.Message) {
	f.t.Helper()
	if _, err := f.sess.Append(session.EventUserMessage, msg, &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: session.SurfaceAppend}}); err != nil {
		f.t.Fatalf("append baseline: %v", err)
	}
}

// runPreStep drives the pre-step waterfall with the claimed batch as the
// base decision.
func (f *testFixture) runPreStep(step int64, claimed ...llm.Message) agent.PreStepDecision {
	f.t.Helper()
	return f.registry.Events().PreStep().Dispatch(f.agent.Scope, agent.PreStepPayload{
		Agent:    f.agent,
		Messages: claimed,
		Step:     step,
		Signal:   context.Background(),
	}, func(agent.PreStepPayload) agent.PreStepDecision {
		return agent.PreStepEnter(claimed)
	})
}

// touchWrite fires a successful `write` execution through the runtime so the
// plugin's tools/result listener projects the touched path.
func (f *testFixture) touchWrite(path string) {
	f.t.Helper()
	definition, defineErr := tools.DefineTool(tools.DefineToolOptions{
		Name:        "write",
		Description: "test write",
		Parameters: map[string]tools.PropSpec{
			"file_path": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}, Required: true},
		},
		Output: tools.ToolOutput{
			Schema: &tools.ValueSchemaSpec{Type: "object", AdditionalProperties: &openSchema},
			Render: func(args map[string]any, value any) []llm.ContentBlock {
				return []llm.ContentBlock{{Type: llm.BlockText, Text: "wrote"}}
			},
		},
		Execute: func(args map[string]any, exec *tools.ToolRunContext) (any, error) {
			return map[string]any{"ok": true}, nil
		},
	})
	if defineErr != nil {
		f.t.Fatalf("define write: %v", defineErr)
	}
	if _, regErr := f.runtime.Register(definition); regErr != nil {
		f.t.Fatalf("register write: %v", regErr)
	}
	result := f.runtime.Execute(&tools.ToolExecutionInput{
		CallID:    "call-1",
		Name:      "write",
		Arguments: map[string]any{"file_path": path},
		Agent:     f.agent.Scope,
		Signal:    context.Background(),
	})
	if result.IsError {
		f.t.Fatalf("write execution failed: %+v / %+v", result.Error, result.Content)
	}
}

// blockText concatenates the text blocks of one message.
func blockText(blocks []llm.ContentBlock) string {
	var builder strings.Builder
	for _, block := range blocks {
		if block.Type == llm.BlockText {
			builder.WriteString(block.Text)
		}
	}
	return builder.String()
}

func (f *testFixture) pendingContexts() []llm.Message {
	f.t.Helper()
	var pending []llm.Message
	for _, message := range f.agent.Inbox.NextStep() {
		if IsWorkspaceContext(message) {
			pending = append(pending, message)
		}
	}
	return pending
}

func contextTexts(decision agent.PreStepDecision) []string {
	texts := make([]string, 0, len(decision.Messages))
	for _, message := range decision.Messages {
		if IsWorkspaceContext(message) {
			texts = append(texts, blockText(message.Content))
		}
	}
	return texts
}

func countWorkspace(messages []llm.Message) int {
	count := 0
	for _, message := range messages {
		if IsWorkspaceContext(message) {
			count++
		}
	}
	return count
}

func TestBaselineComposesUserGlobalRootAndNested(t *testing.T) {
	fixture := newFixture(t)
	decision := fixture.runPreStep(1, userMessage("direct prompt"))
	texts := contextTexts(decision)
	if len(texts) != 1 {
		t.Fatalf("expected exactly one baseline context, got %d", len(texts))
	}
	text := texts[0]
	if !strings.HasPrefix(text, "<system-reminder>\nThe following workspace instructions may be relevant") {
		t.Fatalf("baseline intro mismatch: %q", text[:min(80, len(text))])
	}
	if !strings.HasSuffix(text, "</system-reminder>") {
		t.Fatal("baseline must close the system-reminder frame")
	}
	// The official ancestor chain walks cwd-upward then appends the root,
	// so the user-global instruction renders first, then the nested file,
	// then the project root.
	userGlobalAt := strings.Index(text, "Instructions from:")
	nestedAt := strings.Index(text, "nested instructions")
	rootAt := strings.Index(text, "root instructions")
	if userGlobalAt < 0 || rootAt < 0 || nestedAt < 0 || !(userGlobalAt < nestedAt && nestedAt < rootAt) {
		t.Fatalf("section order mismatch in %q", text)
	}
	var baseline *llm.Message
	for i, message := range decision.Messages {
		if IsWorkspaceContext(message) {
			baseline = &decision.Messages[i]
		}
	}
	if !baseline.Source.Baseline || baseline.Source.BaselineIdentity == "" {
		t.Fatalf("baseline source missing identity: %+v", baseline.Source)
	}
	if len(baseline.Source.Changes) != 3 {
		t.Fatalf("expected 3 set changes, got %d", len(baseline.Source.Changes))
	}
	for _, change := range baseline.Source.Changes {
		if change.Action != "set" || change.Digest == "" {
			t.Fatalf("unexpected change %+v", change)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestResumeReusesVisibleBaseline(t *testing.T) {
	fixture := newFixture(t)
	first := fixture.runPreStep(1, userMessage("turn one"))
	baseline := first.Messages[len(first.Messages)-1]
	fixture.publish(baseline)
	claimed := []llm.Message{baseline, userMessage("turn two")}
	second := fixture.runPreStep(1, claimed...)
	// The claimed batch may legitimately carry the visible baseline; no
	// NEW workspace context may join it.
	extra := 0
	for i, message := range second.Messages {
		if !IsWorkspaceContext(message) {
			continue
		}
		inClaimed := false
		for _, c := range claimed {
			if SameContextPayload(message, c) {
				inClaimed = true
			}
		}
		if !inClaimed {
			extra++
			t.Fatalf("unexpected new context at %d: %q", i, blockText(message.Content))
		}
	}
	if extra != 0 || len(second.Messages) != len(claimed) {
		t.Fatalf("expected exactly the claimed batch back, got %d messages (%d new contexts)", len(second.Messages), extra)
	}
	// A second pass stays silent as well.
	third := fixture.runPreStep(1, claimed...)
	if len(third.Messages) != len(claimed) {
		t.Fatalf("third step must stay silent, got %d messages", len(third.Messages))
	}
}

func TestChangedIdentityReplacesBaseline(t *testing.T) {
	fixture := newFixture(t)
	first := fixture.runPreStep(1, userMessage("turn one"))
	baseline := first.Messages[len(first.Messages)-1]
	fixture.publish(baseline)
	// A different candidate set changes the baseline identity.
	fixture.startPlugin(Config{MaxBytes: 1 << 20, InstructionFileCandidates: []string{"CLAUDE.md"}})
	fixture.startAgent()
	fixture.writeFile(filepath.Join(fixture.project, "CLAUDE.md"), "claude instructions\n")
	second := fixture.runPreStep(1, baseline, userMessage("turn two"))
	texts := contextTexts(second)
	if len(texts) < 2 {
		t.Fatalf("expected the claimed old baseline plus a replacement, got %d", len(texts))
	}
	// texts[0] is the claimed old baseline (standard intro); the replacement
	// announces itself and carries the new candidate content.
	replacementText := texts[1]
	if !strings.Contains(replacementText, "This complete workspace instruction baseline replaces all earlier workspace instruction baselines.") {
		t.Fatalf("replacement intro missing: %q", replacementText)
	}
	if !strings.Contains(replacementText, "claude instructions") {
		t.Fatalf("replacement baseline missing new content: %q", replacementText)
	}
	// Exactly one message carries the new baseline flag; its change set
	// removes the scopes the new candidate set no longer loads.
	var replacement *llm.Message
	for i := range second.Messages {
		if IsWorkspaceContext(second.Messages[i]) && second.Messages[i].Source.Baseline {
			replacement = &second.Messages[i]
		}
	}
	if replacement == nil {
		t.Fatal("no baseline-flagged replacement message")
	}
	hasRemoval := false
	for _, change := range replacement.Source.Changes {
		if change.Action == "remove" && strings.Contains(change.Path, "AGENTS.md") {
			hasRemoval = true
		}
	}
	if !hasRemoval {
		t.Fatalf("replacement must remove vanished scopes: %+v", replacement.Source.Changes)
	}
}

func TestToolTouchProjectsUpdatedInstructions(t *testing.T) {
	fixture := newFixture(t)
	first := fixture.runPreStep(1, userMessage("turn one"))
	fixture.publish(first.Messages[len(first.Messages)-1])
	fixture.writeRoot("root instructions, updated\n")
	fixture.touchWrite(filepath.Join(fixture.project, "other.txt"))
	pending := fixture.pendingContexts()
	if len(pending) != 1 {
		t.Fatalf("expected one pending update context, got %d", len(pending))
	}
	text := blockText(pending[0].Content)
	if !strings.Contains(text, "Updated instructions from: AGENTS.md") {
		t.Fatalf("update section missing: %q", text)
	}
	if !strings.Contains(text, "root instructions, updated") {
		t.Fatalf("update content missing: %q", text)
	}
	// The next pre-step folds the pending context after the claimed batch.
	second := fixture.runPreStep(2, userMessage("turn two"))
	folded := contextTexts(second)
	if len(folded) != 1 || !strings.Contains(folded[0], "Updated instructions from: AGENTS.md") {
		t.Fatalf("pending context did not enter: %v", folded)
	}
	if fixture.agent.Inbox.HasPending() && len(fixture.pendingContexts()) != 0 {
		t.Fatalf("pending context must settle after entering: %d", len(fixture.pendingContexts()))
	}
}

func TestToolTouchProjectsRemoval(t *testing.T) {
	fixture := newFixture(t)
	first := fixture.runPreStep(1, userMessage("turn one"))
	fixture.publish(first.Messages[len(first.Messages)-1])
	if err := os.Remove(filepath.Join(fixture.nested, "AGENTS.md")); err != nil {
		t.Fatalf("remove nested: %v", err)
	}
	fixture.touchWrite(filepath.Join(fixture.project, "other.txt"))
	pending := fixture.pendingContexts()
	if len(pending) != 1 {
		t.Fatalf("expected one removal context, got %d", len(pending))
	}
	text := blockText(pending[0].Content)
	if !strings.Contains(text, "Instructions removed: nested/AGENTS.md") {
		t.Fatalf("removal section missing: %q", text)
	}
}

func TestTinyBudgetOmitsAndTruncates(t *testing.T) {
	fixture := newFixture(t)
	fixture.undo()
	fixture.startPlugin(Config{MaxBytes: 300})
	fixture.startAgent()
	decision := fixture.runPreStep(1, userMessage("direct"))
	texts := contextTexts(decision)
	if len(texts) != 1 {
		t.Fatalf("expected one budget-limited context, got %d", len(texts))
	}
	if !strings.Contains(texts[0], "Workspace instruction budget 300 bytes") {
		t.Fatalf("budget marker missing: %q", texts[0])
	}
}

func TestDisabledBudgetNeverInjects(t *testing.T) {
	fixture := newFixture(t)
	fixture.undo()
	fixture.startPlugin(Config{MaxBytes: 0})
	fixture.startAgent()
	decision := fixture.runPreStep(1, userMessage("direct"))
	if countWorkspace(decision.Messages) != 0 {
		t.Fatal("disabled budget must not inject context")
	}
	if len(fixture.pendingContexts()) != 0 {
		t.Fatal("disabled budget must not queue pending context")
	}
}

func TestSameDirectoryDuplicateCollapses(t *testing.T) {
	fixture := newFixture(t)
	fixture.writeFile(filepath.Join(fixture.project, "CLAUDE.md"), "root instructions\n")
	decision := fixture.runPreStep(1, userMessage("direct"))
	texts := contextTexts(decision)
	if len(texts) != 1 {
		t.Fatalf("expected one baseline, got %d", len(texts))
	}
	if strings.Count(texts[0], "root instructions") != 1 {
		t.Fatalf("duplicate content must render once: %q", texts[0])
	}
}

func TestEmptyFirstStepKeepsPending(t *testing.T) {
	fixture := newFixture(t)
	decision := fixture.runPreStep(1)
	if len(decision.Messages) != 0 {
		t.Fatalf("no-step turn must stay empty, got %d", len(decision.Messages))
	}
	if len(fixture.pendingContexts()) != 1 {
		t.Fatalf("baseline must stay pending for the real step: %d", len(fixture.pendingContexts()))
	}
}

func TestDisposalStopsProjection(t *testing.T) {
	fixture := newFixture(t)
	first := fixture.runPreStep(1, userMessage("turn one"))
	fixture.publish(first.Messages[len(first.Messages)-1])
	fixture.undo()
	fixture.writeRoot("root instructions, updated\n")
	fixture.touchWrite(filepath.Join(fixture.project, "other.txt"))
	if len(fixture.pendingContexts()) != 0 {
		t.Fatal("disposed plugin must not project touches")
	}
	decision := fixture.runPreStep(2, userMessage("turn two"))
	if countWorkspace(decision.Messages) != 0 {
		t.Fatal("disposed plugin must not inject context")
	}
}

func TestRejectPassesThrough(t *testing.T) {
	fixture := newFixture(t)
	decision := fixture.registry.Events().PreStep().Dispatch(fixture.agent.Scope, agent.PreStepPayload{
		Agent:  fixture.agent,
		Step:   1,
		Signal: context.Background(),
	}, func(agent.PreStepPayload) agent.PreStepDecision {
		return agent.PreStepReject()
	})
	if decision.Kind != "reject" {
		t.Fatalf("reject must pass through, got %q", decision.Kind)
	}
}

func TestDigests(t *testing.T) {
	content := "abc\n"
	digest := InstructionContentSha1(content)
	// SHA-1("abc\n") hex.
	if digest != "03cfd743661f07975fa2f1220c5194cbaff48451" {
		t.Fatalf("sha1 mismatch: %s", digest)
	}
	if TrimmedInstructionDigest("  abc\n ") != InstructionContentSha1("abc") {
		t.Fatal("trimmed digest must hash trimmed content")
	}
	if TrimmedInstructionDigest("") != "da39a3ee5e6b4b0d3255bfef95601890afd80709" {
		t.Fatal("empty digest mismatch")
	}
}

func TestTruncateUtf8Boundary(t *testing.T) {
	value := "aé中\U0001F600"
	// Cut through the continuation bytes of 中 (3-byte sequence at 2..4).
	got := truncateUtf8(value, 3)
	if got != "aé" {
		t.Fatalf("cut through multibyte: %q", got)
	}
	if truncateUtf8(value, int64(len(value))) != value {
		t.Fatal("within-budget cut must be identity")
	}
	if truncateUtf8(value, 1) != "a" {
		t.Fatalf("ascii cut mismatch: %q", truncateUtf8(value, 1))
	}
}

func TestScopeKeysRoundTrip(t *testing.T) {
	key := CandidateScopeKey(UserGlobalDirectory, "AGENTS.md")
	decoded := DecodeScopeKey(key)
	if decoded.Directory != UserGlobalDirectory || decoded.CandidateName != "AGENTS.md" {
		t.Fatalf("round trip mismatch: %+v", decoded)
	}
	if ScopeForDisplayPath("~/.dsh/AGENTS.md") != UserGlobalDirectory {
		t.Fatal("user-global display path must map to the user-global scope")
	}
	if ScopeForDisplayPath("nested/dir/AGENTS.md") != "nested/dir" {
		t.Fatalf("project scope mismatch: %q", ScopeForDisplayPath("nested/dir/AGENTS.md"))
	}
	if InstructionScopeKey("nested/AGENTS.md") != CandidateScopeKey("nested", "AGENTS.md") {
		t.Fatal("instruction scope key mismatch")
	}
}

func TestDirectoryChains(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// The official chain walks cwd-upward then appends the root: most
	// specific first.
	chain := AncestorChain(root, nested)
	if len(chain) != 3 || chain[0] != nested || chain[2] != root {
		t.Fatalf("chain mismatch: %v", chain)
	}
	touched := filepath.Join(nested, "file.txt")
	descendants := DescendantDirsBetween(root, touched)
	if len(descendants) != 2 || descendants[0] != filepath.Join(root, "a") || descendants[1] != root {
		t.Fatalf("descendants mismatch: %v", descendants)
	}
	if outside := DescendantDirsBetween(nested, filepath.Join(root, "x.txt")); len(outside) != 0 {
		t.Fatalf("out-of-root touch must yield nothing: %v", outside)
	}
}

func TestWorkspaceBaselineIdentitySerialization(t *testing.T) {
	resolved := ResolveConfig(Config{MaxBytes: 100, DSHHome: t.TempDir()})
	identity := WorkspaceBaselineIdentity(resolved, `D:\proj`, `D:\proj\sub`)
	if !strings.Contains(identity, `"projectRoot":"sub"`) {
		t.Fatalf("identity missing relative root: %s", identity)
	}
	other := WorkspaceBaselineIdentity(resolved, `D:\proj`, `D:\proj`)
	if identity == other {
		t.Fatal("different roots must yield different identities")
	}
}

package toolskill

import (
	"context"
	"strings"
	"testing"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/skill"
	"dshgo/tools"
)

// stubProvider serves a fixed catalog.
type stubProvider struct {
	name        string
	candidates  []skill.Candidate
	definitions map[string]*skill.Definition
	control     skill.ProviderControl
	invalidate  func()
}

func (p *stubProvider) Name() string { return p.name }

func (p *stubProvider) List(options skill.LookupOptions) (skill.ProviderObservation, error) {
	return skill.ProviderObservation{Candidates: p.candidates, Complete: true}, nil
}

func (p *stubProvider) Get(candidate skill.Candidate, options skill.LookupOptions) (*skill.Definition, error) {
	if p.definitions == nil {
		return nil, nil
	}
	return p.definitions[candidate.Name], nil
}

func summaryOf(name string, model bool, user bool) skill.Summary {
	return skill.Summary{
		Name:        name,
		Description: "the " + name + " skill",
		Invocation:  skill.InvocationPolicy{ModelInvocable: model, UserInvocable: user},
		Source:      "test",
		Provider:    "test",
	}
}

func definitionOf(summary skill.Summary) *skill.Definition {
	return &skill.Definition{Summary: summary, Content: "full instructions for " + summary.Name}
}

// testRig wires a registry, a skill registry with one provider, a runtime,
// and one live agent whose session cwd is set.
type testRig struct {
	registry     *agent.AgentRegistry
	skills       *skill.Registry
	runtime      *tools.ToolRuntime
	toolUndo     func()
	agent        *agent.Agent
	sess         *session.Session
	provider     *stubProvider
	providerUndo func()
}

func newRig(t *testing.T, config Config) *testRig {
	t.Helper()
	rig := &testRig{}
	rig.registry = agent.NewAgentRegistry(nil, nil)
	registry, err := skill.NewRegistry(cordis.Discard{}, skill.Config{})
	if err != nil {
		t.Fatalf("skill registry: %v", err)
	}
	rig.skills = registry
	rig.provider = &stubProvider{name: "test"}
	rig.providerUndo, err = rig.skills.RegisterProviderIn(nil, func(control skill.ProviderControl) (skill.Provider, error) {
		rig.provider.control = control
		return rig.provider, nil
	})
	if err != nil {
		t.Fatalf("register provider: %v", err)
	}
	rig.runtime, err = tools.NewToolRuntime(nil, tools.Config{})
	if err != nil {
		t.Fatalf("tool runtime: %v", err)
	}
	undo, err := Register(rig.runtime, rig.skills, rig.registry, nil, config)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	rig.toolUndo = undo

	sess, err := session.NewDetached(session.SessionID("agent-1"), nil, &session.SessionHeader{ID: session.SessionID("agent-1"), CWD: "D:\\proj"}, 0)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	inbox, err := agent.NewInbox(sess, nil)
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	built := agent.NewAgent(agent.AgentConfig{ID: sess.ID(), Session: sess, Inbox: inbox}, rig.registry.Events())
	if _, err := rig.registry.Enter(built, nil); err != nil {
		t.Fatalf("enter: %v", err)
	}
	rig.agent = built
	rig.sess = sess
	t.Cleanup(func() {
		rig.toolUndo()
		rig.providerUndo()
	})
	return rig
}

func (r *testRig) publish(names ...skill.Summary) {
	r.provider.candidates = make([]skill.Candidate, 0, len(names))
	for _, summary := range names {
		entry := skill.Candidate{Summary: summary, Rank: 500}
		if summary.Name == "deploy" {
			entry.Locator = "deploy-locator"
		}
		r.provider.candidates = append(r.provider.candidates, entry)
	}
	// A provider-driven catalog change reaches the registry only through
	// control.Invalidate, exactly as the official provider reports drift.
	if r.provider.control.Invalidate != nil {
		r.provider.control.Invalidate()
	}
}

func (r *testRig) setDefinition(summary skill.Summary) {
	if r.provider.definitions == nil {
		r.provider.definitions = map[string]*skill.Definition{}
	}
	r.provider.definitions[summary.Name] = definitionOf(summary)
}

func userMessage(text string) llm.Message {
	return llm.NewUserMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: text}}, llm.MessageSource{Kind: llm.SourceUser})
}

func step(claimed ...llm.Message) agent.PreStepDecision {
	return agent.PreStepEnter(claimed)
}

func (r *testRig) runPreStep(t *testing.T, claimed ...llm.Message) agent.PreStepDecision {
	t.Helper()
	return r.registry.Events().PreStep().Dispatch(r.agent.Scope, agent.PreStepPayload{
		Agent:    r.agent,
		Messages: claimed,
		Signal:   context.Background(),
	}, func(agent.PreStepPayload) agent.PreStepDecision {
		return step(claimed...)
	})
}

func (r *testRig) loadSkill(t *testing.T, name string) (any, error) {
	t.Helper()
	definition, ok := r.runtime.Get(Name, r.agent.Scope)
	if !ok {
		t.Fatal("skill tool not visible")
	}
	return definition.Execute(map[string]any{"name": name}, &tools.ToolRunContext{
		ToolExecution: tools.ToolExecution{Agent: r.agent.Scope},
		Signal:        context.Background(),
	})
}

func mustRuntime(t *testing.T) *tools.ToolRuntime {
	t.Helper()
	runtime, err := tools.NewToolRuntime(nil, tools.Config{})
	if err != nil {
		t.Fatalf("tool runtime: %v", err)
	}
	return runtime
}

func TestConfigValidation(t *testing.T) {
	if _, err := Register(nil, nil, nil, nil, Config{}); err == nil || !strings.Contains(err.Error(), "tool-skill: a tool runtime is required") {
		t.Fatalf("nil runtime accepted: %v", err)
	}
	if _, err := Register(mustRuntime(t), nil, nil, nil, Config{}); err == nil || !strings.Contains(err.Error(), "tool-skill: a skill registry is required") {
		t.Fatalf("nil skills accepted: %v", err)
	}
	registry, err := skill.NewRegistry(cordis.Discard{}, skill.Config{})
	if err != nil {
		t.Fatalf("skill registry: %v", err)
	}
	if _, err := Register(mustRuntime(t), registry, nil, nil, Config{CatalogDescriptionMaxLength: 2}); err == nil ||
		!strings.Contains(err.Error(), "tool-skill: catalogDescriptionMaxLength must be an integer greater than or equal to 3") {
		t.Fatalf("bad max length accepted: %v", err)
	}
}

func TestToolLoadsModelInvocableSkill(t *testing.T) {
	rig := newRig(t, Config{})
	rig.publish(summaryOf("deploy", true, true), summaryOf("hidden", false, true))
	rig.setDefinition(summaryOf("deploy", true, true))

	value, err := rig.loadSkill(t, "deploy")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	loaded, ok := value.(LoadedSkill)
	if !ok || loaded.Name != "deploy" || loaded.Content != "full instructions for deploy" {
		t.Fatalf("loaded = %+v", value)
	}

	if _, err := rig.loadSkill(t, "nope"); err == nil || !strings.Contains(err.Error(), `skill "nope" is unknown or no longer available`) {
		t.Fatalf("unknown accepted: %v", err)
	}
	if _, err := rig.loadSkill(t, "Bad_Name"); err == nil || !strings.Contains(err.Error(), `invalid skill name "Bad_Name"`) {
		t.Fatalf("bad grammar accepted: %v", err)
	}
	// A model-disabled skill is invisible to the tool even though it lists.
	rig.publish(summaryOf("deploy", true, true), summaryOf("hidden", false, true))
	rig.setDefinition(summaryOf("hidden", false, true))
	if _, err := rig.loadSkill(t, "hidden"); err == nil || !strings.Contains(err.Error(), `skill "hidden" is not available for model invocation`) {
		t.Fatalf("model-disabled loaded: %v", err)
	}
}

func TestFirstPublicationAppendsCatalogAndReuseStaysSilent(t *testing.T) {
	rig := newRig(t, Config{})
	rig.publish(summaryOf("deploy", true, false))

	decision := rig.runPreStep(t, userMessage("hello"))
	if len(decision.Messages) != 2 {
		t.Fatalf("messages = %d", len(decision.Messages))
	}
	catalog := decision.Messages[1]
	if catalog.Source.Kind != SourceKindSkillCatalog || catalog.Source.Form != llm.FormCatalog {
		t.Fatalf("source = %+v", catalog.Source)
	}
	if len(catalog.Source.CatalogEntries) != 1 || catalog.Source.CatalogEntries[0].Name != "deploy" {
		t.Fatalf("entries = %+v", catalog.Source.CatalogEntries)
	}
	if !strings.Contains(catalog.Content[0].Text, "<available_skills>") ||
		!strings.Contains(catalog.Content[0].Text, "- `deploy`: the deploy skill") ||
		!strings.Contains(catalog.Content[0].Text, "call the `skill` tool with the exact skill name") {
		t.Fatalf("catalog text = %s", catalog.Content[0].Text)
	}
	if strings.Contains(catalog.Content[0].Text, "replaces every earlier") {
		t.Fatalf("first publication rendered as update: %s", catalog.Content[0].Text)
	}

	// The loop would persist the catalog; simulate that so history sees it.
	if _, err := rig.sess.Append(session.EventUserMessage, catalog, &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: session.SurfaceAppend}}); err != nil {
		t.Fatalf("append: %v", err)
	}

	// The same digest with the durable copy visible: silent.
	decision = rig.runPreStep(t, userMessage("again"))
	if len(decision.Messages) != 1 {
		t.Fatalf("second step messages = %d", len(decision.Messages))
	}
	if decision.Messages[0].ID == catalog.ID {
		t.Fatal("catalog re-admitted despite stable digest")
	}
}

func TestCatalogChangeReplacesInPlace(t *testing.T) {
	rig := newRig(t, Config{})
	rig.publish(summaryOf("deploy", true, false))
	first := rig.runPreStep(t, userMessage("hello"))
	if len(first.Messages) != 2 || first.Messages[1].Source.Kind != SourceKindSkillCatalog {
		t.Fatalf("first = %+v", first.Messages)
	}
	if _, err := rig.sess.Append(session.EventUserMessage, first.Messages[1], &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: session.SurfaceAppend}}); err != nil {
		t.Fatalf("append: %v", err)
	}

	// A new skill changes the entries: the replacement rides the same slot.
	rig.publish(summaryOf("deploy", true, false), summaryOf("review", true, false))
	second := rig.runPreStep(t, userMessage("more"))
	if len(second.Messages) != 2 {
		t.Fatalf("second = %+v", second.Messages)
	}
	replacement := second.Messages[1]
	if replacement.Source.Kind != SourceKindSkillCatalog || !replacement.Source.CatalogUpdate {
		t.Fatalf("replacement source = %+v", replacement.Source)
	}
	if len(replacement.Source.CatalogEntries) != 2 {
		t.Fatalf("replacement entries = %+v", replacement.Source.CatalogEntries)
	}
	if !strings.Contains(replacement.Content[0].Text, "The available skill catalog changed") ||
		!strings.Contains(replacement.Content[0].Text, "replaces every earlier available-skills list") {
		t.Fatalf("replacement text = %s", replacement.Content[0].Text)
	}
	if _, err := rig.sess.Append(session.EventUserMessage, replacement, &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: session.SurfaceAppend}}); err != nil {
		t.Fatalf("append 2: %v", err)
	}

	// Stability after the replacement: silent.
	third := rig.runPreStep(t, userMessage("steady"))
	if len(third.Messages) != 1 || third.Messages[0].Source.Kind == SourceKindSkillCatalog {
		t.Fatalf("third = %+v", third.Messages)
	}
}

func TestEmptyFirstCatalogIsSilentAndLaterWithdraws(t *testing.T) {
	rig := newRig(t, Config{})
	decision := rig.runPreStep(t, userMessage("hello"))
	if len(decision.Messages) != 1 {
		t.Fatalf("empty catalog published: %+v", decision.Messages)
	}
	// Once published, an empty replacement withdraws the offer.
	rig.publish(summaryOf("deploy", true, false))
	first := rig.runPreStep(t, userMessage("hello"))
	if len(first.Messages) != 2 || first.Messages[1].Source.Kind != SourceKindSkillCatalog {
		t.Fatalf("first = %+v", first.Messages)
	}
	if _, err := rig.sess.Append(session.EventUserMessage, first.Messages[1], &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: session.SurfaceAppend}}); err != nil {
		t.Fatalf("append: %v", err)
	}
	rig.publish()
	second := rig.runPreStep(t, userMessage("gone"))
	if len(second.Messages) != 2 {
		t.Fatalf("withdraw messages = %+v", second.Messages)
	}
	withdraw := second.Messages[1]
	if !withdraw.Source.CatalogUpdate || len(withdraw.Source.CatalogEntries) != 0 {
		t.Fatalf("withdraw source = %+v", withdraw.Source)
	}
	if !strings.Contains(withdraw.Content[0].Text, "No skills are currently available through the `skill` tool") {
		t.Fatalf("withdraw text = %s", withdraw.Content[0].Text)
	}
}

func TestGestureLoadsUserInvocableSkillAsInstructions(t *testing.T) {
	rig := newRig(t, Config{})
	rig.publish(summaryOf("deploy", true, true), summaryOf("manual-only", false, true), summaryOf("passive", true, false))
	rig.setDefinition(summaryOf("deploy", true, true))
	rig.setDefinition(summaryOf("manual-only", false, true))

	decision := rig.runPreStep(t, userMessage("please /deploy the service"), userMessage("and /review /manual-only too"))
	var injected []llm.Message
	for _, message := range decision.Messages {
		if message.Source.Kind == SourceKindSkillInvocation {
			injected = append(injected, message)
		}
	}
	// /deploy resolves (model+user), /review is unknown prose, and
	// /manual-only loads through the gesture even though the model could
	// never invoke it.
	if len(injected) != 2 {
		t.Fatalf("injected = %d", len(injected))
	}
	if injected[0].Source.Summary != "deploy" || injected[1].Source.Summary != "manual-only" {
		t.Fatalf("order = %q, %q", injected[0].Source.Summary, injected[1].Source.Summary)
	}
	if injected[0].Source.Form != llm.FormInstructions {
		t.Fatalf("form = %q", injected[0].Source.Form)
	}
	if !strings.Contains(injected[0].Content[0].Text, `<skill_content name="deploy">`) ||
		!strings.Contains(injected[0].Content[0].Text, "full instructions for deploy") {
		t.Fatalf("gesture text = %s", injected[0].Content[0].Text)
	}
	// Injections land after the claimed batch: material last.
	if !strings.Contains(decision.Messages[0].Content[0].Text, "/deploy the service") {
		t.Fatalf("claimed message not first: %+v", decision.Messages[0])
	}

	// A model-invocation-disabled skill the gesture does not name stays out;
	// passive never loads without its own gesture.
	for _, message := range decision.Messages {
		if message.Source.Kind == SourceKindSkillInvocation && message.Source.Summary == "passive" {
			t.Fatal("passive skill loaded without gesture")
		}
	}
}

func TestGestureIgnoresNonUserSourcesAndPaths(t *testing.T) {
	rig := newRig(t, Config{})
	rig.publish(summaryOf("deploy", true, true))
	plugin := llm.NewUserMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: "/deploy /usr/bin 5/8"}}, llm.MessageSource{Kind: llm.SourcePlugin, Plugin: "other"})
	decision := rig.runPreStep(t, plugin)
	for _, message := range decision.Messages {
		if message.Source.Kind == SourceKindSkillInvocation {
			t.Fatal("plugin message forged a gesture")
		}
	}
	// Path and fraction tokens in a user message stay prose; the catalog
	// still publishes beside them.
	decision = rig.runPreStep(t, userMessage("check /usr/bin and 5/8 but not /deploy-later-"))
	for _, message := range decision.Messages {
		if message.Source.Kind == SourceKindSkillInvocation {
			t.Fatalf("path token loaded a skill: %+v", message.Source)
		}
	}
	if len(decision.Messages) != 2 {
		t.Fatalf("messages = %d", len(decision.Messages))
	}
}

func TestCatalogRequiresExactToolVisibility(t *testing.T) {
	rig := newRig(t, Config{})
	rig.publish(summaryOf("deploy", true, false))
	// A restriction that hides the skill tool removes the catalog guidance
	// with it.
	restrict, err := rig.runtime.RestrictIn(rig.agent.Scope, nil, []string{Name})
	if err != nil {
		t.Fatalf("restrict: %v", err)
	}
	defer restrict()
	decision := rig.runPreStep(t, userMessage("hello"))
	if len(decision.Messages) != 1 {
		t.Fatalf("catalog published despite hidden tool: %+v", decision.Messages)
	}
}

func TestRejectDecisionPassesThrough(t *testing.T) {
	rig := newRig(t, Config{})
	rig.publish(summaryOf("deploy", true, false))
	decision := rig.registry.Events().PreStep().Dispatch(rig.agent.Scope, agent.PreStepPayload{
		Agent:  rig.agent,
		Signal: context.Background(),
	}, func(agent.PreStepPayload) agent.PreStepDecision {
		return agent.PreStepReject()
	})
	if decision.Kind != "reject" {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestCatalogDescriptionNormalization(t *testing.T) {
	long := strings.Repeat("word ", 300)
	entry := catalogDescription(strings.Join([]string{"a", "  \n b", long}, "\t"), 30)
	if entry != strings.Join([]string{"a", "b", strings.TrimSpace(long[:30-3-1])}, " ")[:30] && len([]rune(entry)) != 30 {
		t.Fatalf("normalized = %q", entry)
	}
	if !strings.HasSuffix(entry, "...") {
		t.Fatalf("ellipsis missing: %q", entry)
	}
	simple := catalogDescription("  spaced   out  ", 500)
	if simple != "spaced out" {
		t.Fatalf("simple = %q", simple)
	}
}

func TestDisposeRemovesCatalogAndTool(t *testing.T) {
	rig := newRig(t, Config{})
	rig.publish(summaryOf("deploy", true, false))
	if _, ok := rig.runtime.Get(Name, rig.agent.Scope); !ok {
		t.Fatal("tool missing before dispose")
	}
	rig.toolUndo()
	if _, ok := rig.runtime.Get(Name, rig.agent.Scope); ok {
		t.Fatal("tool survived dispose")
	}
	decision := rig.runPreStep(t, userMessage("hello"))
	if len(decision.Messages) != 1 {
		t.Fatalf("catalog published after dispose: %+v", decision.Messages)
	}
	// A gesture after disposal stays prose.
	decision = rig.runPreStep(t, userMessage("/deploy now"))
	if len(decision.Messages) != 1 {
		t.Fatalf("gesture after dispose: %+v", decision.Messages)
	}
}

func TestDigestIsStableAndInputSensitive(t *testing.T) {
	a := digestCatalogEntries([]catalogEntry{{Name: "x", Description: "y"}})
	b := digestCatalogEntries([]catalogEntry{{Name: "x", Description: "y"}})
	if a != b {
		t.Fatal("digest unstable")
	}
	c := digestCatalogEntries([]catalogEntry{{Name: "x,y", Description: "z"}})
	d := digestCatalogEntries([]catalogEntry{{Name: "x", Description: "y,z"}})
	if c == d {
		t.Fatal("digest collided across separator spellings")
	}
}

func TestCancellationLeavesDecisionUntouched(t *testing.T) {
	rig := newRig(t, Config{})
	rig.publish(summaryOf("deploy", true, false))
	ctxCanceled, cancel := context.WithCancel(context.Background())
	cancel()
	decision := rig.registry.Events().PreStep().Dispatch(rig.agent.Scope, agent.PreStepPayload{
		Agent:  rig.agent,
		Signal: ctxCanceled,
	}, func(agent.PreStepPayload) agent.PreStepDecision {
		return step(userMessage("hello"))
	})
	if len(decision.Messages) != 1 {
		t.Fatalf("canceled step touched: %+v", decision.Messages)
	}
}

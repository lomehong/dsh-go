package boot

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"dshgo/cordis"
	"dshgo/cordis/loader"
	"dshgo/fs"
	"dshgo/interaction/permissionpresets"
	"dshgo/session"
	"dshgo/session/persistence"
	"dshgo/settings"
	"dshgo/shell"
	"dshgo/subagent"
	"dshgo/toolresultpruner"
	"dshgo/tools"
	"dshgo/typert"
)

func TestCatalogResolvesOfficialNames(t *testing.T) {
	home := t.TempDir()
	resolver := NewCatalog(CatalogDeps{Logger: cordis.Discard{}, Home: home})

	ctx := cordis.NewRoot(cordis.Discard{})
	entries := []loader.Entry{
		{ID: "tools", Name: "@deepseek-ai/dsh-tools"},
		{ID: "commands", Name: "@deepseek-ai/dsh-commands"},
		{ID: "settings", Name: "@deepseek-ai/dsh-settings-file"},
		{ID: "credentials", Name: "@deepseek-ai/dsh-credentials-local"},
		{ID: "web", Name: "@deepseek-ai/dsh-web"},
	}
	for _, entry := range entries {
		spec, err := resolver(entry.Name)
		if err != nil {
			t.Fatalf("resolve %s: %v", entry.Name, err)
		}
		if err := ctx.Inject(spec.Inject, func(injected *cordis.Context) error {
			return spec.Apply(injected, entry.Config)
		}); err != nil {
			t.Fatalf("apply %s: %v", entry.Name, err)
		}
	}

	for _, service := range []string{ServiceTools, ServiceCommands, ServiceSettings, ServiceCredential, ServiceWebServer} {
		if ctx.Get(service) == nil {
			t.Fatalf("service %q missing after assembly", service)
		}
	}
	if err := ctx.Dispose(); err != nil {
		t.Fatalf("dispose: %v", err)
	}
}

func TestCatalogAssemblesCoreServicesThroughAssemble(t *testing.T) {
	home := t.TempDir()
	root := cordis.NewRoot(cordis.Discard{})
	app, err := Assemble(root, []loader.Entry{
		{ID: "tools", Name: "@deepseek-ai/dsh-tools"},
		{ID: "commands", Name: "@deepseek-ai/dsh-commands"},
		{ID: "settings", Name: "@deepseek-ai/dsh-settings-file"},
		{ID: "credentials", Name: "@deepseek-ai/dsh-credentials-local"},
		{ID: "web", Name: "@deepseek-ai/dsh-web"},
		{ID: "typert", Name: "@deepseek-ai/dsh-typert-registry"},
		{ID: "sessions", Name: "@deepseek-ai/dsh-session"},
		{ID: "projections", Name: "@deepseek-ai/dsh-session-projection"},
		{ID: "agents", Name: "@deepseek-ai/dsh-agent"},
		{ID: "llm", Name: "@deepseek-ai/dsh-llm"},
		{ID: "deepseek", Name: "@deepseek-ai/dsh-llm-deepseek"},
		{ID: "persistence", Name: "@deepseek-ai/dsh-session-persistence-jsonl"},
		{ID: "user-questions", Name: "@deepseek-ai/dsh-user-questions"},
		{ID: "user-approval", Name: "@deepseek-ai/dsh-user-approval"},
		{ID: "permission-presets", Name: "@deepseek-ai/dsh-permission-presets"},
		{ID: "system-prompt", Name: "@deepseek-ai/dsh-system-prompt"},
		{ID: "agent-loop", Name: "@deepseek-ai/dsh-agent-loop"},
		{ID: "subagent", Name: "@deepseek-ai/dsh-subagent"},
		{ID: "spawn", Name: "@deepseek-ai/dsh-subagent-spawn-in-process"},
		{ID: "fork", Name: "@deepseek-ai/dsh-subagent-fork-in-process"},
	}, NewCatalog(CatalogDeps{Logger: cordis.Discard{}, Home: home}))
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	ctx := root
	for _, service := range []string{
		ServiceTools, ServiceCommands, ServiceSettings, ServiceCredential, ServiceWebServer,
		ServiceSessions, ServiceProjections, ServiceAgents, ServiceLlm, ServiceSessionPersist,
		ServiceUserQuestions, ServiceUserApproval, ServicePermissionPresets,
		ServiceSystemPrompt, ServiceAgentLoop, ServiceSubagentRuntime,
	} {
		if ctx.Get(service) == nil {
			t.Fatalf("service %q missing after Assemble", service)
		}
	}
	if err := app.Shutdown(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestCatalogPermissionPresetsSettingsSectionAndCreationHook(t *testing.T) {
	home := t.TempDir()
	root := cordis.NewRoot(cordis.Discard{})
	app, err := Assemble(root, []loader.Entry{
		{ID: "settings", Name: "@deepseek-ai/dsh-settings-file"},
		{ID: "typert", Name: "@deepseek-ai/dsh-typert-registry"},
		{ID: "sessions", Name: "@deepseek-ai/dsh-session"},
		{ID: "permission-presets", Name: "@deepseek-ai/dsh-permission-presets"},
	}, NewCatalog(CatalogDeps{Logger: cordis.Discard{}, Home: home}))
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	settingsStore := root.Get(ServiceSettings).(*settings.Store)
	if !settingsStore.HasNamespace("permission") {
		t.Fatal("permission settings section missing after Assemble")
	}
	// The persisted user section overrides the composition default for
	// sessions created after the change.
	if err := settingsStore.ProviderPush("permission", map[string]any{"defaultPreset": "danger-full-access"}); err != nil {
		t.Fatalf("provider push: %v", err)
	}
	sessions := root.Get(ServiceSessions).(*session.Store)
	overridden, err := sessions.Create("perm-hook-overridden", session.CreateOptions{})
	if err != nil {
		t.Fatalf("create overridden: %v", err)
	}
	if selected, ok := permissionpresets.EffectivePermissionPreset(overridden.Events()); !ok || selected != "danger-full-access" {
		t.Fatalf("settings-driven default = %q (ok %v)", selected, ok)
	}

	// The composition default still applies while the user section holds.
	if err := settingsStore.ProviderPush("permission", map[string]any{"defaultPreset": "workspace-write"}); err != nil {
		t.Fatalf("provider push back: %v", err)
	}
	fresh, err := sessions.Create("perm-hook-fresh", session.CreateOptions{})
	if err != nil {
		t.Fatalf("create fresh: %v", err)
	}
	if selected, ok := permissionpresets.EffectivePermissionPreset(fresh.Events()); !ok || selected != "workspace-write" {
		t.Fatalf("composition default = %q (ok %v)", selected, ok)
	}
	if err := app.Shutdown(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestCatalogRegistersInProcessProviders(t *testing.T) {
	home := t.TempDir()
	root := cordis.NewRoot(cordis.Discard{})
	runtimeSpec := func(name string) loader.Entry {
		return loader.Entry{ID: name, Name: "@deepseek-ai/dsh-" + name}
	}
	entries := []loader.Entry{}
	for _, name := range []string{
		"tools", "commands", "settings-file", "credentials-local", "web",
		"typert-registry", "session", "session-projection", "agent", "llm", "llm-deepseek",
		"session-persistence-jsonl", "user-questions", "user-approval",
		"permission-presets", "system-prompt", "agent-loop", "subagent",
		"skill", "tool-skill", "tool-todo", "jobs-local", "tool-jobs",
		"plan-mode", "repeat-tool-reminder", "token-meter",
		"compaction-tool-result-pruner", "compaction-basic", "command-compact",
		"tool-subagent-control",
	} {
		entries = append(entries, runtimeSpec(name))
	}
	entries = append(entries,
		loader.Entry{ID: "spawn", Name: "@deepseek-ai/dsh-subagent-spawn-in-process"},
		loader.Entry{ID: "fork", Name: "@deepseek-ai/dsh-subagent-fork-in-process"},
		loader.Entry{ID: "sandbox-policy", Name: "@deepseek-ai/dsh-sandbox-policy"},
		loader.Entry{ID: "fs-sandbox", Name: "@deepseek-ai/dsh-fs-sandbox"},
		loader.Entry{ID: "editor", Name: "@deepseek-ai/dsh-tool-str-replace-editor"},
		loader.Entry{ID: "shell-env", Name: "@deepseek-ai/dsh-shell-env"},
		loader.Entry{ID: "tool-fs", Name: "@deepseek-ai/dsh-tool-fs"},
		loader.Entry{ID: "tool-fs-search", Name: "@deepseek-ai/dsh-tool-fs-search"},
		loader.Entry{ID: "subprocess", Name: "@deepseek-ai/dsh-subprocess"},
	)
	// One shell executor + one model-facing shell tool per host: the
	// official win32 layer swaps the bash rows for the pwsh ones.
	if runtime.GOOS == "windows" {
		entries = append(entries,
			loader.Entry{ID: "pwsh", Name: "@deepseek-ai/dsh-pwsh-local"},
			loader.Entry{ID: "tool-pwsh", Name: "@deepseek-ai/dsh-tool-pwsh"})
	} else {
		entries = append(entries,
			loader.Entry{ID: "bash", Name: "@deepseek-ai/dsh-bash-local"},
			loader.Entry{ID: "tool-bash", Name: "@deepseek-ai/dsh-tool-bash"})
	}
	app, err := Assemble(root, entries, NewCatalog(CatalogDeps{Logger: cordis.Discard{}, Home: home}))
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	runtime := root.Get(ServiceSubagentRuntime).(*subagent.SubagentRuntime)
	spawn, ok := runtime.GetProvider("spawn")
	if !ok || spawn.Name() != "spawn" {
		t.Fatalf("spawn provider missing (got %v, %v)", spawn, ok)
	}
	fork, ok := runtime.GetProvider("fork")
	if !ok || !fork.InheritsParentContext() {
		t.Fatal("fork provider missing or wrong context contract")
	}
	if fork.Capabilities().OutputSchema {
		t.Fatal("fork must not advertise outputSchema before the structured round")
	}
	for _, service := range []string{ServiceSkills, ServiceJobs, ServicePlanMode, ServiceTokenMeter, ServiceCompaction, ServiceToolResultPruner, ServiceSandboxPolicy, ServiceFS, ServiceSubprocess} {
		if root.Get(service) == nil {
			t.Fatalf("service %q missing after Assemble", service)
		}
	}
	// The pruner entry mounted before compaction-basic composes into the
	// engine's optional prune pass: the resolved budgets are the plugin's
	// defaults.
	pruner := root.Get(ServiceToolResultPruner).(*toolresultpruner.Pruner)
	if pruner.Config() != toolresultpruner.Defaults {
		t.Fatalf("pruner budgets: %+v", pruner.Config())
	}
	toolRuntime := root.Get(ServiceTools).(*tools.ToolRuntime)
	for _, name := range []string{"read", "write", "edit", "glob", "grep"} {
		if _, ok := toolRuntime.Get(name, nil); !ok {
			t.Fatalf("tool %q missing after Assemble", name)
		}
	}
	// The model-facing shell tool follows the composed executor flavor.
	shellExecutor := root.Get(ServiceShell).(shell.ShellExecutor)
	shellTool := "bash"
	if shellExecutor.Name() == "pwsh-local" {
		shellTool = "pwsh"
	}
	if _, ok := toolRuntime.Get(shellTool, nil); !ok {
		t.Fatalf("shell tool %q missing after Assemble", shellTool)
	}
	// Under a confining backend the escalation fields are advertised.
	schema := toolRuntime.Schemas(nil)
	found := false
	for _, tool := range schema {
		if tool.Name == "write" {
			found = true
			properties, _ := tool.Parameters["properties"].(map[string]any)
			if properties == nil || properties["sandbox_permissions"] == nil || properties["justification"] == nil {
				t.Fatalf("write must advertise escalation fields under a confining backend: %+v", tool.Parameters)
			}
		}
	}
	if !found {
		t.Fatal("write schema missing from Schemas()")
	}
	if err := app.Shutdown(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestCatalogSandboxCompositionFencesEditor(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	root := cordis.NewRoot(cordis.Discard{})
	entries := []loader.Entry{
		{ID: "tools", Name: "@deepseek-ai/dsh-tools"},
		{ID: "commands", Name: "@deepseek-ai/dsh-commands"},
		{ID: "settings", Name: "@deepseek-ai/dsh-settings-file"},
		{ID: "credentials", Name: "@deepseek-ai/dsh-credentials-local"},
		{ID: "web", Name: "@deepseek-ai/dsh-web"},
		{ID: "typert", Name: "@deepseek-ai/dsh-typert-registry"},
		{ID: "sessions", Name: "@deepseek-ai/dsh-session"},
		{ID: "projections", Name: "@deepseek-ai/dsh-session-projection"},
		{ID: "agents", Name: "@deepseek-ai/dsh-agent"},
		{ID: "llm", Name: "@deepseek-ai/dsh-llm"},
		{ID: "persistence", Name: "@deepseek-ai/dsh-session-persistence-jsonl"},
		{ID: "user-questions", Name: "@deepseek-ai/dsh-user-questions"},
		{ID: "user-approval", Name: "@deepseek-ai/dsh-user-approval"},
		{ID: "permission-presets", Name: "@deepseek-ai/dsh-permission-presets"},
		{ID: "system-prompt", Name: "@deepseek-ai/dsh-system-prompt"},
		{ID: "agent-loop", Name: "@deepseek-ai/dsh-agent-loop"},
		{ID: "sandbox-policy", Name: "@deepseek-ai/dsh-sandbox-policy", Config: map[string]any{"mode": "workspace-write"}},
		{ID: "fs-sandbox", Name: "@deepseek-ai/dsh-fs-sandbox", Config: map[string]any{"cwd": workspace}},
		{ID: "editor", Name: "@deepseek-ai/dsh-tool-str-replace-editor"},
	}
	app, err := Assemble(root, entries, NewCatalog(CatalogDeps{Logger: cordis.Discard{}, Home: home}))
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	defer func() {
		if err := app.Shutdown(); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	}()
	for _, service := range []string{ServiceSandboxPolicy, ServiceFS} {
		if root.Get(service) == nil {
			t.Fatalf("service %q missing after Assemble", service)
		}
	}
	runtime := root.Get(ServiceTools).(*tools.ToolRuntime)
	definition, ok := runtime.Get("str_replace_editor", nil)
	if !ok {
		t.Fatal("editor must be registered over the sandboxed backend")
	}
	execute := func(args map[string]any) (any, error) {
		return definition.Execute(args, &tools.ToolRunContext{})
	}
	// Inside the workspace root: create succeeds and the file lands.
	inside := filepath.Join(workspace, "doc.txt")
	if _, err := execute(map[string]any{"command": "create", "path": inside, "file_text": "seed"}); err != nil {
		t.Fatalf("inside-root create: %v", err)
	}
	stored, err := os.ReadFile(inside)
	if err != nil || string(stored) != "seed" {
		t.Fatalf("stored: %q, %v", string(stored), err)
	}
	// Outside every writable root (the drive root): FS_SANDBOX_DENIED,
	// wrapped by the editor's sandbox denial marker.
	volume := filepath.VolumeName(workspace)
	deniedPath := volume + string(os.PathSeparator) + "dsh-sandbox-denied-target.txt"
	if volume == "" {
		deniedPath = "/dsh-sandbox-denied-target.txt"
	}
	outside := deniedPath
	_, err = execute(map[string]any{"command": "create", "path": outside, "file_text": "x"})
	if codeErr, ok := err.(*fs.Error); !ok || codeErr.Code != fs.CodeSandboxDenied {
		t.Fatalf("outside-root create must be FS_SANDBOX_DENIED: %v", err)
	}
	if !strings.Contains(err.Error(), "file access denied under workspace-write mode") {
		t.Fatalf("denial text: %v", err)
	}
	if !strings.Contains(err.Error(), "The edit was denied by the sandbox") {
		t.Fatalf("the editor must wrap the denial with its marker: %v", err)
	}
}

func TestCatalogSettingsConfigOverridesPath(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(t.TempDir(), "custom.yaml")
	resolver := NewCatalog(CatalogDeps{Logger: cordis.Discard{}, Home: home})

	spec, err := resolver("@deepseek-ai/dsh-settings-file")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	ctx := cordis.NewRoot(cordis.Discard{})
	if err := spec.Apply(ctx, map[string]any{"path": path}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if ctx.Get(ServiceSettings) == nil {
		t.Fatal("settings store missing")
	}
	if err := ctx.Dispose(); err != nil {
		t.Fatalf("dispose: %v", err)
	}
}

func TestCatalogMissFailsLoud(t *testing.T) {
	resolver := NewCatalog(CatalogDeps{Logger: cordis.Discard{}, Home: t.TempDir()})
	// The typert loader stays unported (node type stripping — recorded);
	// the miss must still be loud.
	_, err := resolver("@deepseek-ai/dsh-typert-loader")
	if err == nil || !strings.Contains(err.Error(), "module not found") {
		t.Fatalf("err = %v, want a loud module-not-found miss", err)
	}
}

func TestCatalogAssemblesSqlitePersistenceAndRoundTrips(t *testing.T) {
	home := t.TempDir()
	root := cordis.NewRoot(cordis.Discard{})
	app, err := Assemble(root, []loader.Entry{
		{ID: "typert", Name: "@deepseek-ai/dsh-typert-registry"},
		{ID: "sessions", Name: "@deepseek-ai/dsh-session"},
		{ID: "persistence", Name: "@deepseek-ai/dsh-session-persistence-sqlite",
			Config: map[string]any{"path": filepath.Join(home, "store", "sessions.db")}},
	}, NewCatalog(CatalogDeps{Logger: cordis.Discard{}, Home: home}))
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	coordinator, ok := root.Get(ServiceSessionPersist).(*persistence.Coordinator)
	if !ok || coordinator == nil {
		t.Fatalf("service %q is not a persistence.Coordinator", ServiceSessionPersist)
	}
	// A live session materializes through the sqlite backend and lists back.
	store := root.Get(ServiceSessions).(*session.Store)
	sess, err := store.Create("sqlite-live", session.CreateOptions{})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := coordinator.EnsureMaterialized(sess); err != nil {
		t.Fatalf("ensure materialized: %v", err)
	}
	snapshots, err := coordinator.ListSnapshots()
	if err != nil {
		t.Fatalf("list snapshots: %v", err)
	}
	if len(snapshots) != 1 || snapshots[0].Header.ID != "sqlite-live" {
		t.Fatalf("snapshots = %+v", snapshots)
	}
	if err := app.Shutdown(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	// The database file exists under the requested path (parent dirs made).
	if _, err := os.Stat(filepath.Join(home, "store", "sessions.db")); err != nil {
		t.Fatalf("database file missing: %v", err)
	}
}

func TestCatalogTypertRegistryAndLookupRegistration(t *testing.T) {
	home := t.TempDir()
	root := cordis.NewRoot(cordis.Discard{})
	app, err := Assemble(root, []loader.Entry{
		{ID: "typert", Name: "@deepseek-ai/dsh-typert-registry"},
		{ID: "sessions", Name: "@deepseek-ai/dsh-session"},
		{ID: "agents", Name: "@deepseek-ai/dsh-agent"},
	}, NewCatalog(CatalogDeps{Logger: cordis.Discard{}, Home: home}))
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	registry, ok := typert.ContextService.From(root)
	if !ok {
		t.Fatal("typert service missing after assembly")
	}
	// The session and agent lookups register through their entries.
	for _, key := range []string{"session", "agent"} {
		if _, found := registry.LookupGet(key); !found {
			t.Fatalf("lookup %q missing after assembly", key)
		}
	}
	if _, ok := registry.ContextGetHost("agent"); !ok {
		t.Fatal("agent host context adapter missing after assembly")
	}
	// The session lookup resolves a live store session by wire id.
	store := root.Get(ServiceSessions).(*session.Store)
	if _, err := store.Create("typert-live", session.CreateOptions{}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	lookup, _ := registry.LookupGet("session")
	resolved, err := lookup.Resolve("typert-live")
	if err != nil || resolved == nil {
		t.Fatalf("session lookup resolve = %v, %v", resolved, err)
	}
	if absent, err := lookup.Resolve("absent"); err != nil || absent != nil {
		t.Fatalf("absent resolve = %v, %v (want nil/nil)", absent, err)
	}
	// Disposal withdraws the entry-registered lookups.
	if err := app.Shutdown(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if _, found := registry.LookupGet("session"); found {
		t.Fatal("session lookup must withdraw on disposal")
	}
	if _, found := registry.LookupGet("agent"); found {
		t.Fatal("agent lookup must withdraw on disposal")
	}
}

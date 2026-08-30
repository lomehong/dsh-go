package boot

import (
	"errors"
	"strings"
	"testing"

	"dshgo/cordis"
	"dshgo/cordis/loader"
)

// catalog resolves two in-memory plugins: one recorder and one service
// provider.
func catalog(t *testing.T, records *[]string, provided *string) PluginResolver {
	t.Helper()
	return func(name string) (PluginSpec, error) {
		switch name {
		case "dsh/recorder":
			return PluginSpec{Inject: []string{"missing-service"}}, errors.New("unreachable")
		case "dsh/echo":
			return PluginSpec{Apply: func(ctx *cordis.Context, config any) error {
				*records = append(*records, "echo:"+configString(config))
				return nil
			}}, nil
		case "dsh/provider":
			return PluginSpec{Inject: []string{}, Provide: []string{"greeting"}, Apply: func(ctx *cordis.Context, config any) error {
				ctx.Provide("greeting", "hello")
				return nil
			}}, nil
		case "dsh/consumer":
			return PluginSpec{Inject: []string{"greeting"}, Apply: func(ctx *cordis.Context, config any) error {
				*records = append(*records, "consumer:"+ctx.Get("greeting").(string))
				return nil
			}}, nil
		default:
			return PluginSpec{}, errors.New("module not found")
		}
	}
}

func configString(config any) string {
	if text, ok := config.(string); ok {
		return text
	}
	return "none"
}

func TestAssembleMountsEntriesWithInjection(t *testing.T) {
	var records []string
	var greeting string
	root := cordis.NewRoot(cordis.Discard{})
	entries := []loader.Entry{
		{ID: "p", Name: "dsh/provider"},
		{ID: "c", Name: "dsh/consumer"},
		{ID: "e", Name: "dsh/echo", Config: "configured"},
	}
	app, err := Assemble(root, entries, catalog(t, &records, &greeting))
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	// The consumer applied only after the provider's service arrived.
	if len(records) != 2 || records[0] != "consumer:hello" || records[1] != "echo:configured" {
		t.Fatalf("records = %v", records)
	}
	_ = greeting
	if err := app.Shutdown(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestAssembleSkipsDisabledSubtrees(t *testing.T) {
	var records []string
	root := cordis.NewRoot(cordis.Discard{})
	entries := []loader.Entry{
		{ID: "off", Name: "dsh/echo", Disabled: true, Group: true, Config: []loader.Entry{
			{ID: "inner", Name: "dsh/echo"},
		}},
		{ID: "on", Name: "dsh/echo"},
	}
	app, err := Assemble(root, entries, catalog(t, &records, nil))
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	// The disabled group and its child never resolve; the enabled entry does.
	if len(records) != 1 {
		t.Fatalf("records = %v", records)
	}
	_ = app
}

func TestAssembleGroupBuildsChildScope(t *testing.T) {
	var records []string
	root := cordis.NewRoot(cordis.Discard{})
	entries := []loader.Entry{
		{ID: "grp", Name: "group", Group: true, Config: []loader.Entry{
			{ID: "p", Name: "dsh/provider"},
			{ID: "c", Name: "dsh/consumer"},
		}},
	}
	app, err := Assemble(root, entries, catalog(t, &records, nil))
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if len(records) != 1 || records[0] != "consumer:hello" {
		t.Fatalf("records = %v", records)
	}
	if err := app.Shutdown(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	// Shutdown disposes the child scope too: the root's disposers ran (the
	// consumer's mount was registered on the child; root dispose surfaces
	// through the app teardown completing without error).
}

func TestAssembleEntryInjectOverridesPlugin(t *testing.T) {
	var records []string
	root := cordis.NewRoot(cordis.Discard{})
	// dsh/consumer requires greeting; the entry overrides inject to
	// nothing, so it applies immediately with no service.
	entries := []loader.Entry{
		{ID: "c", Name: "dsh/consumer", Inject: []string{}},
	}
	_, err := Assemble(root, entries, func(name string) (PluginSpec, error) {
		if name == "dsh/consumer" {
			return PluginSpec{Inject: []string{"greeting"}, Apply: func(ctx *cordis.Context, config any) error {
				records = append(records, "consumer:none")
				return nil
			}}, nil
		}
		return PluginSpec{}, errors.New("module not found")
	})
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %v", records)
	}
}

func TestAssembleFailsLoudOnUnknownModule(t *testing.T) {
	root := cordis.NewRoot(cordis.Discard{})
	entries := []loader.Entry{{ID: "x", Name: "dsh/ghost"}}
	if _, err := Assemble(root, entries, func(name string) (PluginSpec, error) {
		return PluginSpec{}, errors.New("module not found")
	}); err == nil || !strings.Contains(err.Error(), "failed to import loader entry x (dsh/ghost)") {
		t.Fatalf("err = %v", err)
	}
}

func TestAssembleFailsLoudOnInvalidDisabled(t *testing.T) {
	root := cordis.NewRoot(cordis.Discard{})
	entries := []loader.Entry{{ID: "x", Name: "dsh/echo", Disabled: "sometimes"}}
	if _, err := Assemble(root, entries, func(name string) (PluginSpec, error) {
		return PluginSpec{}, errors.New("module not found")
	}); err == nil || !strings.Contains(err.Error(), "failed to apply loader entry x") {
		t.Fatalf("err = %v", err)
	}
}

func TestAppPendingInjectionsAuditsRootAndGroups(t *testing.T) {
	waiter := func(name string) (PluginSpec, error) {
		if name == "dsh/waiter" {
			return PluginSpec{Inject: []string{"absent"}, Apply: func(ctx *cordis.Context, config any) error {
				return nil
			}}, nil
		}
		return PluginSpec{}, errors.New("module not found")
	}
	root := cordis.NewRoot(cordis.Discard{})
	entries := []loader.Entry{
		{ID: "top", Name: "dsh/waiter"},
		{ID: "grp", Name: "group", Group: true, Config: []loader.Entry{
			{ID: "nested", Name: "dsh/waiter"},
		}},
	}
	app, err := Assemble(root, entries, waiter)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	pending := app.PendingInjections()
	if len(pending) != 2 {
		t.Fatalf("pending = %v", pending)
	}
	for _, set := range pending {
		if len(set) != 1 || set[0] != "absent" {
			t.Fatalf("pending set = %v", set)
		}
	}
	if err := app.Dispose(); err != nil {
		t.Fatalf("dispose: %v", err)
	}
}

func TestMountableResolverRejectsUnflaggedPlugins(t *testing.T) {
	inner := func(name string) (PluginSpec, error) {
		switch name {
		case "dsh/plain":
			return PluginSpec{}, nil
		case "dsh/safe":
			return PluginSpec{Mountable: true}, nil
		default:
			return PluginSpec{}, errors.New("module not found")
		}
	}
	resolver := mountableResolver(inner)
	if _, err := resolver("dsh/plain"); err == nil || !strings.Contains(err.Error(), "not mountable into preset compositions") {
		t.Fatalf("unflagged plugin = %v", err)
	}
	spec, err := resolver("dsh/safe")
	if err != nil || !spec.Mountable {
		t.Fatalf("flagged plugin = %+v, %v", spec, err)
	}
	if _, err := resolver("dsh/ghost"); err == nil {
		t.Fatal("unknown plugin passed the mountable resolver")
	}
}

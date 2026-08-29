package boot

import (
	"path/filepath"
	"strings"
	"testing"

	"dshgo/cordis"
	"dshgo/cordis/loader"
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
	_, err := resolver("@deepseek-ai/dsh-typert-registry")
	if err == nil || !strings.Contains(err.Error(), "module not found") {
		t.Fatalf("err = %v, want a loud module-not-found miss", err)
	}
}

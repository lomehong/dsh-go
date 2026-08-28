package homepaths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveDshHomePrecedence(t *testing.T) {
	home := t.TempDir()
	osHome = func() (string, error) { return home, nil }
	t.Cleanup(func() { osHome = os.UserHomeDir })
	// The harness sets DSH_HOME for itself; tests drive the seam directly.
	blankEnv := func(string) string { return "" }

	// Default: ~/.dsh under the OS home.
	want := filepath.Join(home, ".dsh")
	if got := ResolveDshHome("", blankEnv); got != want {
		t.Fatalf("default = %q, want %q", got, want)
	}
	// Blank $DSH_HOME is unset, never cwd.
	if got := ResolveDshHome("", func(string) string { return "   " }); got != want {
		t.Fatalf("blank env = %q, want the default home", got)
	}
	// $DSH_HOME wins over the default.
	if got := ResolveDshHome("", func(string) string { return "/custom/dsh-home-test" }); !filepath.IsAbs(got) || !strings.HasSuffix(got, "dsh-home-test") {
		t.Fatalf("env = %q, want the custom root", got)
	}
	// Configured beats $DSH_HOME.
	if got := ResolveDshHome("/configured-home-test", func(string) string { return "/env" }); !strings.HasSuffix(got, "configured-home-test") {
		t.Fatalf("configured = %q", got)
	}
}

func TestExpandHomePath(t *testing.T) {
	home := homeDir()
	cases := map[string]string{
		"~":          home,
		"~/.dsh":     filepath.Join(home, ".dsh"),
		`~\x`:        filepath.Join(home, "x"),
		"/plain":     "/plain",
		"~notatilde": "~notatilde",
		"~/":         home,
	}
	for input, want := range cases {
		if got := ExpandHomePath(input); got != want {
			t.Fatalf("ExpandHomePath(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestDshHomeDisplay(t *testing.T) {
	if display := DshHomeDisplay(ResolveDshHome("", func(string) string { return "" })); display != "~/.dsh" {
		t.Fatalf("default display = %q", display)
	}
	if display := DshHomeDisplay(ResolveDshHome("", func(string) string { return "/elsewhere-home-test" })); display != "$DSH_HOME" {
		t.Fatalf("configured display = %q", display)
	}
}

func TestDefaultDshHomeUnderUserHome(t *testing.T) {
	if !strings.HasSuffix(DefaultDshHome(), ".dsh") {
		t.Fatalf("default = %q", DefaultDshHome())
	}
}

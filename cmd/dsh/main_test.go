package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// serveWebBody extracts the serveWeb function source so the guard can
// assert it never re-introduces a listener (r92 root cause).
func serveWebBody(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	src := string(raw)
	start := strings.Index(src, "func serveWeb(")
	if start < 0 {
		t.Fatal("serveWeb not found in main.go")
	}
	next := strings.Index(src[start:], "\nfunc ")
	if next < 0 {
		next = len(src) - start
	}
	return src[start : start+next]
}

// TestServeWebDoesNotListen is the v3 guard's launcher half (single listen
// holder): serveWeb mounts the dist fallback over the catalog-bound
// registry and must never call Listen itself — the pre-r92 double bind was
// serveWeb's own web.Listen colliding with the catalog webserver row.
func TestServeWebDoesNotListen(t *testing.T) {
	if m := regexp.MustCompile(`\.Listen\(`).FindString(serveWebBody(t)); m != "" {
		t.Fatalf("serveWeb must not listen (catalog webserver row owns the bind): found %q", m)
	}
}

func TestWebAliasResolvesPositionalWeb(t *testing.T) {
	profile, web := webAlias([]string{"web"})
	if !web || profile != "web" {
		t.Fatalf("webAlias = %q %v", profile, web)
	}
}

func TestWebAliasResolvesExplicitProfileFlag(t *testing.T) {
	profile, web := webAlias([]string{"--profile", "web"})
	if !web || profile != "web" {
		t.Fatalf("webAlias = %q %v", profile, web)
	}
}

func TestWebAliasRejectsHeadless(t *testing.T) {
	for _, args := range [][]string{
		{"headless"},
		{"--profile", "headless"},
	} {
		if profile, web := webAlias(args); web {
			t.Fatalf("webAlias(%v) = %q web=true, want false", args, profile)
		}
	}
}

func TestWebAliasIgnoresInnerWebPosition(t *testing.T) {
	// The official launcher treats only a leading `web` as the alias; a web
	// token after flags belongs to the inner arguments.
	if profile, web := webAlias([]string{"--profile", "headless", "web"}); web {
		t.Fatalf("webAlias = %q web=true, want false", profile)
	}
}

func TestWebDefaultsMatchOfficialFallbacks(t *testing.T) {
	if defaultWebHost != "127.0.0.1" {
		t.Fatalf("defaultWebHost = %q", defaultWebHost)
	}
	if defaultWebPort != "3080" {
		t.Fatalf("defaultWebPort = %q", defaultWebPort)
	}
}

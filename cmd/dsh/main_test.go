package main

import (
	"testing"
)

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

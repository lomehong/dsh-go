package scope

import (
	"strings"
	"testing"
)

func TestBindParentLinksAfterMinting(t *testing.T) {
	parent := NewScopeKey(nil)
	child := NewScopeKey(nil)
	if got := ParentOf(child); got != nil {
		t.Fatalf("unbound key has parent %v", got)
	}
	if _, err := BindParent(child, parent); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if got := ParentOf(child); got != parent {
		t.Fatalf("ParentOf = %v, want %v", got, parent)
	}
	chain := ChainOf(child)
	if len(chain) != 2 || chain[0] != child || chain[1] != parent {
		t.Fatalf("ChainOf = %v", chain)
	}
}

func TestBindParentRejectsRebindVerbatim(t *testing.T) {
	parent := NewScopeKey(nil)
	child := NewScopeKey(nil)
	if _, err := BindParent(child, parent); err != nil {
		t.Fatalf("bind: %v", err)
	}
	_, err := BindParent(child, NewScopeKey(nil))
	want := "dsh-scope: scope key is already bound to a parent; re-linking requires the binding returned by the original bind"
	if err == nil || err.Error() != want {
		t.Fatalf("rebind refusal = %v, want %q", err, want)
	}
}

func TestBindingRebindRelinksTheChain(t *testing.T) {
	first := NewScopeKey(nil)
	second := NewScopeKey(nil)
	child := NewScopeKey(nil)
	binding, err := BindParent(child, first)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	binding.Rebind(second)
	if got := ParentOf(child); got != second {
		t.Fatalf("ParentOf = %v, want %v", got, second)
	}
}

func TestBindParentRejectsNilKey(t *testing.T) {
	_, err := BindParent(nil, NewScopeKey(nil))
	if err == nil || !strings.Contains(err.Error(), "cannot bind a nil scope key") {
		t.Fatalf("nil refusal = %v", err)
	}
}

func TestAdmitsFollowsBoundAncestry(t *testing.T) {
	standing := NewScopeKey(nil)
	agentKey := NewScopeKey(nil)
	if _, err := BindParent(agentKey, standing); err != nil {
		t.Fatalf("bind: %v", err)
	}
	// A listener tagged with the standing key receives events dispatched
	// to a joined agent; a tag below the dispatch key stays excluded.
	if !Admits(standing, agentKey) {
		t.Fatal("standing tag not admitted at joined agent")
	}
	descendant := NewScopeKey(agentKey)
	if Admits(descendant, agentKey) {
		t.Fatal("descendant tag admitted above its dispatch key")
	}
	if Admits(NewScopeKey(nil), agentKey) {
		t.Fatal("unrelated tag admitted")
	}
}

func TestParentOfNilKey(t *testing.T) {
	if got := ParentOf(nil); got != nil {
		t.Fatalf("ParentOf(nil) = %v", got)
	}
}

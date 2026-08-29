package persistence

import (
	"dshgo/session"
	"testing"
)

func TestStoreSessionsAdaptsTheRegistrySeam(t *testing.T) {
	store := session.NewStore(nil)
	adapter := NewSessionsAdapter(store)

	created, err := store.Create("s-1", session.CreateOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if got, ok := adapter.Get("s-1"); !ok || got != created {
		t.Fatalf("Get = (%v, %v), want the created session", got, ok)
	}
	if _, ok := adapter.Get("missing"); ok {
		t.Fatal("Get(missing) reported presence")
	}

	live := adapter.List()
	if len(live) != 1 || live[0] != created {
		t.Fatalf("List = %v, want one session", live)
	}

	seed := []session.Event{}
	prepared, err := adapter.Prepare("s-2", seed, session.SessionHeader{ID: "s-2"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if prepared.ID() != "s-2" {
		t.Fatalf("prepared id = %q, want s-2", prepared.ID())
	}
	if _, ok := adapter.Get("s-2"); ok {
		t.Fatal("prepared session must stay unpublished")
	}
}

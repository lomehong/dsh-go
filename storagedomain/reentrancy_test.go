package storagedomain

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestListenerMayReenterDomain covers the R6 contract: dispatch runs outside
// the state mutex, so a listener may synchronously read a snapshot AND land
// a nested write — the official single-threaded emit allows reentrancy, and
// a non-reentrant Go mutex would deadlock without the out-of-lock queue.
func TestListenerMayReenterDomain(t *testing.T) {
	_, _, domain := mustFacility(t, testSpec(), nil, json.RawMessage(`{"n":0}`))

	var mu sync.Mutex
	var order []string
	nestedDone := make(chan struct{})

	first := domain.OnChanged(func(change DomainChanged) {
		mu.Lock()
		order = append(order, fmt.Sprintf("recv %s/%s", change.Table, change.Key))
		mu.Unlock()
		if change.Table == "things" && change.Key == "outer" {
			// Reentrant read: must not deadlock.
			entries := domain.Table("things").Entries()
			if _, ok := entries["outer"]; !ok {
				t.Error("the listener must see the committed record")
			}
			// Reentrant write: must not deadlock; its event rides the same
			// queue and lands after the current one.
			if err := domain.Table("others").Put("inner", json.RawMessage(`{"id":"inner"}`)); err != nil {
				t.Errorf("nested put: %v", err)
			}
			mu.Lock()
			order = append(order, "nested put returned")
			mu.Unlock()
			close(nestedDone)
		}
	})
	defer first()

	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := domain.Table("things").Put("outer", json.RawMessage(`{"id":"outer"}`)); err != nil {
			t.Errorf("outer put: %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the write deadlocked — dispatch must run outside the state mutex")
	}
	select {
	case <-nestedDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the nested write deadlocked")
	}

	// Commit order governs delivery order: outer's event first, then the
	// nested inner write's event.
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(order, " | ")
	want := "recv things/outer | nested put returned | recv others/inner"
	if joined != want {
		t.Fatalf("order = %q, want %q", joined, want)
	}
	if domain.Table("others").Get("inner") == nil {
		t.Fatal("the nested write must be durable in memory")
	}
}

// TestDispatchOrderFollowsCommitOrder pins the queue's FIFO discipline under
// back-to-back writes.
func TestDispatchOrderFollowsCommitOrder(t *testing.T) {
	_, _, domain := mustFacility(t, testSpec(), nil, json.RawMessage(`{"n":0}`))

	var mu sync.Mutex
	var keys []string
	undo := domain.OnChanged(func(change DomainChanged) {
		mu.Lock()
		keys = append(keys, change.Key)
		mu.Unlock()
	})
	defer undo()

	for _, key := range []string{"a", "b", "c"} {
		if err := domain.Table("things").Put(key, json.RawMessage(fmt.Sprintf(`{"id":%q}`, key))); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(keys, ",") != "a,b,c" {
		t.Fatalf("delivery = %v, want commit order a,b,c", keys)
	}
}

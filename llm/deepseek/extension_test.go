package deepseek

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestRegisterValidatesFieldNames(t *testing.T) {
	registry := NewExtensionRegistry()
	for _, field := range []string{"", " spaced", "tabbed\t", " two "} {
		if _, err := registry.Register(field, FieldProviderFunc(func(RequestFacts) (FieldValue, bool, error) { return FieldValue{}, false, nil })); err == nil ||
			!strings.Contains(err.Error(), "field must be a non-blank trimmed string") {
			t.Fatalf("field %q accepted: %v", field, err)
		}
	}
	if _, err := registry.Register("dsh_session_log", nil); err == nil {
		t.Fatal("nil provider accepted")
	}
}

func TestRegisterRejectsDuplicateAndDisposerReleases(t *testing.T) {
	registry := NewExtensionRegistry()
	provider := FieldProviderFunc(func(RequestFacts) (FieldValue, bool, error) { return FieldValue{}, false, nil })
	detach, err := registry.Register("dsh_session_log", provider)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := registry.Register("dsh_session_log", provider); err == nil ||
		!strings.Contains(err.Error(), `field "dsh_session_log" is already registered`) {
		t.Fatalf("duplicate accepted: %v", err)
	}
	detach()
	// After release the field is free again, and the released provider no
	// longer prepares.
	detach2, err := registry.Register("dsh_session_log", FieldProviderFunc(func(RequestFacts) (FieldValue, bool, error) {
		return FieldValue{Value: "v2"}, true, nil
	}))
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	defer detach2()
	prepared, err := registry.Prepare(context.Background(), RequestFacts{})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if prepared.Fields["dsh_session_log"] != "v2" {
		t.Fatalf("fields = %+v", prepared.Fields)
	}
}

func TestPrepareMergesSkipsUndefinedAndAborts(t *testing.T) {
	registry := NewExtensionRegistry()
	detach, err := registry.Register("alpha", FieldProviderFunc(func(request RequestFacts) (FieldValue, bool, error) {
		if request.SessionID == "" {
			return FieldValue{}, false, nil
		}
		return FieldValue{Value: map[string]any{"afterSeq": -1}}, true, nil
	}))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer detach()
	detachB, err := registry.Register("beta", FieldProviderFunc(func(RequestFacts) (FieldValue, bool, error) {
		return FieldValue{Value: 7}, true, nil
	}))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer detachB()

	// No session id: alpha declines, beta still prepares.
	prepared, err := registry.Prepare(context.Background(), RequestFacts{})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, present := prepared.Fields["alpha"]; present {
		t.Fatalf("declined field prepared: %+v", prepared.Fields)
	}
	if prepared.Fields["beta"] != 7 {
		t.Fatalf("fields = %+v", prepared.Fields)
	}
	if err := prepared.Accept(); err != nil {
		t.Fatalf("accept: %v", err)
	}

	// A session id makes alpha prepare; the provider's mutations of the
	// facts body must not leak into the merged request.
	detachC, err := registry.Register("gamma", FieldProviderFunc(func(request RequestFacts) (FieldValue, bool, error) {
		request.Body["hacked"] = true
		return FieldValue{Value: "g"}, true, nil
	}))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer detachC()
	prepared, err = registry.Prepare(context.Background(), RequestFacts{SessionID: "s1", Body: map[string]any{"model": "deepseek-v4-pro"}})
	if err != nil {
		t.Fatalf("prepare 2: %v", err)
	}
	if _, hacked := prepared.Fields["hacked"]; hacked {
		t.Fatal("provider mutation leaked into prepared fields")
	}

	// An aborted signal cancels preparation.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := registry.Prepare(ctx, RequestFacts{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("aborted prepare = %v", err)
	}

	// A provider failure rejects preparation.
	if _, err := registry.Register("boom", FieldProviderFunc(func(RequestFacts) (FieldValue, bool, error) {
		return FieldValue{}, false, errors.New("disk full")
	})); err != nil {
		t.Fatalf("register boom: %v", err)
	}
	if _, err := registry.Prepare(context.Background(), RequestFacts{}); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("provider failure = %v", err)
	}
}

func TestAcceptJoinsSettlementAndAggregatesFailures(t *testing.T) {
	registry := NewExtensionRegistry()
	calls := 0
	var mu sync.Mutex
	detach, err := registry.Register("one", FieldProviderFunc(func(RequestFacts) (FieldValue, bool, error) {
		return FieldValue{Value: 1, Accept: func() error {
			mu.Lock()
			calls++
			mu.Unlock()
			return nil
		}}, true, nil
	}))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer detach()
	if _, err := registry.Register("two", FieldProviderFunc(func(RequestFacts) (FieldValue, bool, error) {
		return FieldValue{Value: 2, Accept: func() error { return errors.New("commit lost") }}, true, nil
	})); err != nil {
		t.Fatalf("register 2: %v", err)
	}
	if _, err := registry.Register("three", FieldProviderFunc(func(RequestFacts) (FieldValue, bool, error) {
		return FieldValue{Value: 3, Accept: func() error { return errors.New("commit gone") }}, true, nil
	})); err != nil {
		t.Fatalf("register 3: %v", err)
	}
	prepared, err := registry.Prepare(context.Background(), RequestFacts{})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	err = prepared.Accept()
	if err == nil || !strings.Contains(err.Error(), "DeepSeek LLM API extension acceptance failed") ||
		!strings.Contains(err.Error(), "commit lost") || !strings.Contains(err.Error(), "commit gone") {
		t.Fatalf("accept = %v", err)
	}
	if calls != 1 {
		t.Fatalf("successful accept ran %d times", calls)
	}
	// Repeated calls join the same settlement: no re-run, same error.
	if err := prepared.Accept(); err == nil {
		t.Fatal("second accept lost the error")
	}
	if calls != 1 {
		t.Fatalf("second accept re-ran callbacks: %d", calls)
	}
}

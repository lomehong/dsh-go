package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"

	"dshgo/cordis"
	"dshgo/typert"
)

// demoService is one business Remote service with lookup, JSON, and
// cancellation-aware methods.
type demoService struct {
	LastSession any
	LastText    string
	LastSignal  context.Context
}

func (s *demoService) Greet(ctx context.Context, session any, text string) (string, error) {
	s.LastSession = session
	s.LastText = text
	s.LastSignal = ctx
	return "hello " + text, nil
}

func (s *demoService) Echo(text string) string { return "hello " + text }

func (s *demoService) Strict(ctx context.Context, text string) string { return "strict " + text }

func (s *demoService) Slow(ctx context.Context) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

func (s *demoService) Plain() string { return "plain" }

func (s *demoService) Booms() (string, error) { return "", errors.New("business exploded") }

func newFixture(t *testing.T) (*cordis.Context, *typert.Registry, *demoService, *Gateway) {
	t.Helper()
	root := cordis.NewRoot(cordis.Discard{})
	registry := typert.NewRegistry(root, cordis.Discard{})
	service := &demoService{}
	root.Provide("demo", service)
	if _, err := registry.Register(typert.Contribution{
		Package: "demo",
		Face:    typert.FaceHost,
		Invocations: []typert.InvocationDescriptor{
			{
				ID: "demo.greet", Service: "demo", Namespace: "demo", Method: "Greet",
				Invocation:            typert.InvocationReceiver{Kind: typert.ReceiverDirect},
				CancellationParameter: "signal",
				Parameters: []typert.InvocationParameterDescriptor{
					{Name: "session", Wire: "sessionId", Source: typert.SourceLookup, Lookup: "session", Codec: typert.Codec{Mode: typert.CodecSrcJSON}},
					{Name: "text", Wire: "text", Source: typert.SourceJSON, Codec: typert.Codec{Mode: typert.CodecSrcJSON}},
				},
				Result: typert.Codec{Mode: typert.CodecSrcJSON},
			},
			{
				ID: "demo.slow", Service: "demo", Namespace: "demo", Method: "Slow",
				Invocation:            typert.InvocationReceiver{Kind: typert.ReceiverDirect},
				CancellationParameter: "signal",
				Result:                typert.Codec{Mode: typert.CodecSrcJSON},
			},
			{
				ID: "demo.plain", Service: "demo", Namespace: "demo", Method: "Plain",
				Invocation: typert.InvocationReceiver{Kind: typert.ReceiverDirect},
				Result:     typert.Codec{Mode: typert.CodecSrcJSON},
			},
			{
				ID: "demo.booms", Service: "demo", Namespace: "demo", Method: "Booms",
				Invocation: typert.InvocationReceiver{Kind: typert.ReceiverDirect},
				Result:     typert.Codec{Mode: typert.CodecSrcJSON},
			},
			{
				ID: "demo.stream", Service: "demo", Namespace: "stream", Method: "Plain", Mode: "stream",
				Invocation: typert.InvocationReceiver{Kind: typert.ReceiverDirect},
				Result:     typert.Codec{Mode: typert.CodecSrcJSON},
			},
		},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := registry.LookupRegister("session", typert.LookupProvider{
		Parameter:      "session",
		Wire:           "sessionId",
		HostTypeSymbol: "Session",
		WireTypeSymbol: "SessionId",
		Resolve: func(id any) (any, error) {
			if id == "s-1" {
				return "live-session", nil
			}
			return nil, nil
		},
	}); err != nil {
		t.Fatalf("register lookup: %v", err)
	}
	gw := New(root, registry)
	t.Cleanup(func() { _ = root.Dispose() })
	return root, registry, service, gw
}

func TestInvokeHappyPathResolvesLookupAndJSON(t *testing.T) {
	_, _, service, gw := newFixture(t)
	result, err := gw.Invoke(context.Background(), InvokeRequest{
		Namespace: "demo", Method: "Greet",
		Args: map[string]any{"sessionId": "s-1", "text": "world"},
	})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if result != "hello world" {
		t.Fatalf("result = %v", result)
	}
	if service.LastSession != "live-session" {
		t.Fatalf("lookup resolution = %v", service.LastSession)
	}
	if service.LastText != "world" {
		t.Fatalf("json parameter = %v", service.LastText)
	}
}

func TestInvokeCancellationRaceMapsToCancelled(t *testing.T) {
	_, _, _, gw := newFixture(t)
	signal, cancel := context.WithCancel(context.Background())
	cancel()
	// The business method observes the injected context and returns its
	// error after the carrier already cancelled: the race maps to the
	// cancelled RPC code through WireFailure.
	_, err := gw.Invoke(signal, InvokeRequest{Namespace: "demo", Method: "Slow", Args: map[string]any{}})
	if err == nil {
		t.Fatal("cancelled slow must report the business error")
	}
	if failure := WireFailure(err); failure.Code != "cancelled" {
		t.Fatalf("race mapping = %+v", failure)
	}
	// A successful business result rides out a stale pre-cancelled signal —
	// the race only exists when the business call itself fails.
	result, err := gw.Invoke(signal, InvokeRequest{
		Namespace: "demo", Method: "Greet",
		Args: map[string]any{"sessionId": "s-1", "text": "steady"},
	})
	if err != nil || result != "hello steady" {
		t.Fatalf("stale signal success = %v, %v", result, err)
	}
	// A business error with a live signal passes through unwrapped.
	_, err = gw.Invoke(context.Background(), InvokeRequest{
		Namespace: "demo", Method: "Booms", Args: map[string]any{},
	})
	if err == nil || err.Error() != "business exploded" {
		t.Fatalf("live-signal business error = %v", err)
	}
	if failure := WireFailure(err); failure.Code != "internal" {
		t.Fatalf("business passthrough mapping = %+v", failure)
	}
}
func TestInvokeWithoutCancellationContextStillInjectsBackground(t *testing.T) {
	_, _, service, gw := newFixture(t)
	if _, err := gw.Invoke(nil, InvokeRequest{
		Namespace: "demo", Method: "Greet",
		Args: map[string]any{"sessionId": "s-1", "text": "x"},
	}); err != nil {
		t.Fatalf("nil signal: %v", err)
	}
	if service.LastSignal == nil {
		t.Fatal("cancellation-aware method must receive a context")
	}
}

func TestInvokePlainMethodHasNoCancellationArgument(t *testing.T) {
	_, _, _, gw := newFixture(t)
	result, err := gw.Invoke(context.Background(), InvokeRequest{
		Namespace: "demo", Method: "Plain", Args: map[string]any{},
	})
	if err != nil || result != "plain" {
		t.Fatalf("plain = %v, %v", result, err)
	}
}

func TestInvokeStreamModeRejectedOnUnaryCarrier(t *testing.T) {
	_, _, _, gw := newFixture(t)
	_, err := gw.Invoke(context.Background(), InvokeRequest{
		Namespace: "stream", Method: "Plain", Args: map[string]any{},
	})
	var gatewayErr *GatewayError
	if !errors.As(err, &gatewayErr) || gatewayErr.Code != CodeSignatureInvalid ||
		!strings.Contains(err.Error(), "stream Remote methods must be opened through the stream carrier") {
		t.Fatalf("stream rejection = %v", err)
	}
}

func TestInvokeArgumentEnvelopeEnforcement(t *testing.T) {
	_, _, _, gw := newFixture(t)
	_, err := gw.Invoke(context.Background(), InvokeRequest{
		Namespace: "demo", Method: "Greet",
		Args: map[string]any{"sessionId": "s-1", "text": "x", "surprise": 1},
	})
	var gatewayErr *GatewayError
	if !errors.As(err, &gatewayErr) || gatewayErr.Code != CodeArgumentsInvalid ||
		!strings.Contains(err.Error(), `unexpected "surprise"`) {
		t.Fatalf("extra arg = %v", err)
	}
	// A strict json parameter may not be absent: register a strict variant.
	strict := cordis.NewRoot(cordis.Discard{})
	defer func() { _ = strict.Dispose() }()
	strictRegistry := typert.NewRegistry(strict, cordis.Discard{})
	strict.Provide("demo", &demoService{})
	if _, err := strictRegistry.Register(typert.Contribution{
		Package: "demo", Face: typert.FaceHost,
		Invocations: []typert.InvocationDescriptor{{
			ID: "demo.strict", Service: "demo", Namespace: "strict", Method: "strict", Implementation: "Strict",
			Invocation:            typert.InvocationReceiver{Kind: typert.ReceiverDirect},
			CancellationParameter: "signal",
			Parameters: []typert.InvocationParameterDescriptor{
				{Name: "text", Wire: "text", Source: typert.SourceJSON, Codec: typert.Codec{Mode: typert.CodecStrict, TypeSymbol: "Text", Validate: func([]byte) error { return nil }}},
			},
			Result: typert.Codec{Mode: typert.CodecSrcJSON},
		}},
	}); err != nil {
		t.Fatalf("register strict: %v", err)
	}
	strictGw := New(strict, strictRegistry)
	_, err = strictGw.Invoke(context.Background(), InvokeRequest{
		Namespace: "strict", Method: "strict",
		Args: map[string]any{"text": "present"},
	})
	if err != nil {
		t.Fatalf("strict present: %v", err)
	}
	_, err = strictGw.Invoke(context.Background(), InvokeRequest{
		Namespace: "strict", Method: "strict",
		Args: map[string]any{},
	})
	if !errors.As(err, &gatewayErr) || !strings.Contains(err.Error(), `missing "text"`) {
		t.Fatalf("missing arg = %v", err)
	}
	// An optional JSON field (acceptsUndefined) may be absent: register a
	// descriptor variant and call it without the field.
	root := cordis.NewRoot(cordis.Discard{})
	defer func() { _ = root.Dispose() }()
	registry := typert.NewRegistry(root, cordis.Discard{})
	service := &demoService{}
	root.Provide("demo", service)
	if _, err := registry.Register(typert.Contribution{
		Package: "demo", Face: typert.FaceHost,
		Invocations: []typert.InvocationDescriptor{{
			ID: "demo.opt", Service: "demo", Namespace: "demo", Method: "Greet",
			Invocation:            typert.InvocationReceiver{Kind: typert.ReceiverDirect},
			CancellationParameter: "signal",
			Parameters: []typert.InvocationParameterDescriptor{
				{Name: "session", Wire: "sessionId", Source: typert.SourceLookup, Lookup: "session", Codec: typert.Codec{Mode: typert.CodecSrcJSON}},
				{Name: "text", Wire: "text", Source: typert.SourceJSON, Codec: typert.Codec{Mode: typert.CodecSrcJSON}, AcceptsUndefined: true},
			},
			Result: typert.Codec{Mode: typert.CodecSrcJSON},
		}},
	}); err != nil {
		t.Fatalf("register optional: %v", err)
	}
	if _, err := registry.LookupRegister("session", typert.LookupProvider{
		Parameter: "session", Wire: "sessionId", HostTypeSymbol: "Session", WireTypeSymbol: "SessionId",
		Resolve: func(any) (any, error) { return "live-session", nil },
	}); err != nil {
		t.Fatalf("register optional lookup: %v", err)
	}
	gw2 := New(root, registry)
	if _, err := gw2.Invoke(context.Background(), InvokeRequest{
		Namespace: "demo", Method: "Greet", Args: map[string]any{"sessionId": "s-1"},
	}); err != nil {
		t.Fatalf("optional absent: %v", err)
	}
}

func TestInvokeDefinitionUnavailableAfterWithdrawal(t *testing.T) {
	root, registry, _, _ := newFixture(t)
	// Withdraw a dedicated contribution by disposing its owning context.
	_ = root
	_ = registry
	child := root.Child()
	childRegistry := typert.NewRegistry(child, cordis.Discard{})
	disposer, err := childRegistry.Register(typert.Contribution{
		Package: "gone", Face: typert.FaceHost,
		Invocations: []typert.InvocationDescriptor{{
			ID: "gone.fly", Service: "demo", Namespace: "gone", Method: "fly", Implementation: "Plain",
			Invocation: typert.InvocationReceiver{Kind: typert.ReceiverDirect},
			Result:     typert.Codec{Mode: typert.CodecSrcJSON},
		}},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	goneGw := New(root, childRegistry)
	disposer()
	_, err = goneGw.Invoke(context.Background(), InvokeRequest{
		Namespace: "gone", Method: "fly", Args: map[string]any{},
	})
	var gatewayErr *GatewayError
	if !errors.As(err, &gatewayErr) || gatewayErr.Code != CodeDefinitionUnavailable ||
		!strings.Contains(err.Error(), "withdrawn and SRC fallback is forbidden") {
		t.Fatalf("withdrawn = %v", err)
	}
	_ = registry
}

func TestInvokeUnknownEndpointInvocationUnavailable(t *testing.T) {
	_, _, _, gw := newFixture(t)
	_, err := gw.Invoke(context.Background(), InvokeRequest{
		Namespace: "absent", Method: "fly", Args: map[string]any{},
	})
	var gatewayErr *GatewayError
	if !errors.As(err, &gatewayErr) || gatewayErr.Code != CodeInvocationUnavailable ||
		!strings.Contains(err.Error(), "no active Remote method exports this endpoint") {
		t.Fatalf("unknown = %v", err)
	}
	_, err = gw.Invoke(context.Background(), InvokeRequest{Namespace: "", Method: "fly"})
	if err == nil || !strings.Contains(err.Error(), `invalid Remote endpoint`) {
		t.Fatalf("malformed endpoint = %v", err)
	}
}

func TestInvokeLookupFailureBranches(t *testing.T) {
	root := cordis.NewRoot(cordis.Discard{})
	defer func() { _ = root.Dispose() }()
	registry := typert.NewRegistry(root, cordis.Discard{})
	root.Provide("demo", &demoService{})
	// One lookup key per branch: a key's wire declaration is frozen for the
	// registry's lifetime, so variants cannot reuse one key.
	descriptor := func(namespace, lookup string) typert.InvocationDescriptor {
		return typert.InvocationDescriptor{
			ID: "demo." + namespace, Service: "demo", Namespace: namespace, Method: "greet", Implementation: "Greet",
			Invocation:            typert.InvocationReceiver{Kind: typert.ReceiverDirect},
			CancellationParameter: "signal",
			Parameters: []typert.InvocationParameterDescriptor{
				{Name: "session", Wire: "sessionId", Source: typert.SourceLookup, Lookup: lookup, Codec: typert.Codec{Mode: typert.CodecStrict, TypeSymbol: "SessionId", Validate: func([]byte) error { return nil }}},
				{Name: "text", Wire: "text", Source: typert.SourceJSON, Codec: typert.Codec{Mode: typert.CodecSrcJSON}},
			},
			Result: typert.Codec{Mode: typert.CodecSrcJSON},
		}
	}
	if _, err := registry.Register(typert.Contribution{
		Package: "demo", Face: typert.FaceHost,
		Invocations: []typert.InvocationDescriptor{
			descriptor("noprovider", "absent-lookup"),
			descriptor("wiremismatch", "wrong-wire"),
			descriptor("symbolmismatch", "wrong-symbol"),
			descriptor("failed", "failing"),
			descriptor("notfound", "empty"),
			descriptor("policy", "policy"),
			descriptor("live", "live"),
		},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	register := func(key string, provider typert.LookupProvider) {
		t.Helper()
		if _, err := registry.LookupRegister(key, provider); err != nil {
			t.Fatalf("register lookup %s: %v", key, err)
		}
	}
	gw := New(root, registry)
	invoke := func(namespace string) error {
		_, err := gw.Invoke(context.Background(), InvokeRequest{
			Namespace: namespace, Method: "greet", Args: map[string]any{"sessionId": "s-1"},
		})
		var gatewayErr *GatewayError
		if err == nil || !errors.As(err, &gatewayErr) {
			t.Fatalf("expected GatewayError for %s, got %v", namespace, err)
		}
		return err
	}

	// Provider absent.
	err := invoke("noprovider")
	var gatewayErr *GatewayError
	errors.As(err, &gatewayErr)
	if gatewayErr.Code != CodeLookupUnavailable {
		t.Fatalf("absent provider = %s", gatewayErr.Code)
	}

	// Wire mismatch against the strict definition.
	register("wrong-wire", typert.LookupProvider{
		Parameter: "session", Wire: "otherId", HostTypeSymbol: "Session", WireTypeSymbol: "SessionId",
		Resolve: func(any) (any, error) { return "x", nil },
	})
	err = invoke("wiremismatch")
	errors.As(err, &gatewayErr)
	if gatewayErr.Code != CodeProviderMismatch {
		t.Fatalf("wire mismatch = %s", gatewayErr.Code)
	}

	// Strict type-symbol mismatch.
	register("wrong-symbol", typert.LookupProvider{
		Parameter: "session", Wire: "sessionId", HostTypeSymbol: "Session", WireTypeSymbol: "Other",
		Resolve: func(any) (any, error) { return "x", nil },
	})
	err = invoke("symbolmismatch")
	errors.As(err, &gatewayErr)
	if gatewayErr.Code != CodeProviderMismatch {
		t.Fatalf("symbol mismatch = %s", gatewayErr.Code)
	}

	// Resolver failure.
	register("failing", typert.LookupProvider{
		Parameter: "session", Wire: "sessionId", HostTypeSymbol: "Session", WireTypeSymbol: "SessionId",
		Resolve: func(any) (any, error) { return nil, errors.New("storage down") },
	})
	err = invoke("failed")
	errors.As(err, &gatewayErr)
	if gatewayErr.Code != CodeLookupFailed {
		t.Fatalf("failed = %s (%v)", gatewayErr.Code, err)
	}
	if !errors.Is(err, context.DeadlineExceeded) && errors.Unwrap(err) == nil {
		t.Fatalf("resolver cause must stay attached: %v", err)
	}

	// Not found.
	register("empty", typert.LookupProvider{
		Parameter: "session", Wire: "sessionId", HostTypeSymbol: "Session", WireTypeSymbol: "SessionId",
		Resolve: func(any) (any, error) { return nil, nil },
	})
	err = invoke("notfound")
	errors.As(err, &gatewayErr)
	if gatewayErr.Code != CodeLookupNotFound {
		t.Fatalf("not found = %s", gatewayErr.Code)
	}

	// LookupFailure keeps its envelope: neither wrapped nor mapped.
	register("policy", typert.LookupProvider{
		Parameter: "session", Wire: "sessionId", HostTypeSymbol: "Session", WireTypeSymbol: "SessionId",
		Resolve: func(any) (any, error) {
			return nil, &typert.LookupFailure{Failure: typert.Failure{Code: "policy-denied", Message: "cold resume"}}
		},
	})
	// The policy branch passes through unwrapped — not a GatewayError.
	_, err = gw.Invoke(context.Background(), InvokeRequest{
		Namespace: "policy", Method: "greet", Args: map[string]any{"sessionId": "s-1"},
	})
	failure := WireFailure(err)
	if failure.Code != "policy-denied" || failure.Message != "cold resume" {
		t.Fatalf("policy passthrough = %+v", failure)
	}

	// The live branch resolves and invokes.
	register("live", typert.LookupProvider{
		Parameter: "session", Wire: "sessionId", HostTypeSymbol: "Session", WireTypeSymbol: "SessionId",
		Resolve: func(any) (any, error) { return "live-session", nil },
	})
	result, err := gw.Invoke(context.Background(), InvokeRequest{
		Namespace: "live", Method: "greet", Args: map[string]any{"sessionId": "s-1"},
	})
	if err != nil || result != "hello " {
		t.Fatalf("live = %v, %v", result, err)
	}
}
func TestContextReceiverHappyAndFailureBranches(t *testing.T) {
	root := cordis.NewRoot(cordis.Discard{})
	defer func() { _ = root.Dispose() }()
	registry := typert.NewRegistry(root, cordis.Discard{})
	agentRegistry := &agentStub{agents: map[string]*cordis.Context{}}
	root.Provide("demo", &demoService{})
	agentContext := root.Child()
	agentContext.Provide("demo", &demoService{})
	agentRegistry.agents["a-1"] = agentContext
	if _, err := registry.ContextRegisterHost("agent", typert.HostContextAdapter{
		Wire: "agentId", WireTypeSymbol: "AgentId",
		Identity: func(any) (any, bool) { return "a-1", true },
		Resolve: func(id any) (any, bool, error) {
			if ctx, ok := agentRegistry.agents[id.(string)]; ok {
				return ctx, true, nil
			}
			return nil, false, nil
		},
	}); err != nil {
		t.Fatalf("register host: %v", err)
	}
	if _, err := registry.Register(typert.Contribution{
		Package: "demo", Face: typert.FaceHost,
		Invocations: []typert.InvocationDescriptor{{
			ID: "scoped.greet", Service: "demo", Namespace: "scoped", Method: "Echo",
			Invocation: typert.InvocationReceiver{
				Kind: typert.ReceiverContext, Context: "agent", Wire: "agentId",
				Codec: typert.Codec{Mode: typert.CodecStrict, TypeSymbol: "AgentId", Validate: func([]byte) error { return nil }},
			},
			Parameters: []typert.InvocationParameterDescriptor{
				{Name: "text", Wire: "text", Source: typert.SourceJSON, Codec: typert.Codec{Mode: typert.CodecSrcJSON}},
			},
			Result: typert.Codec{Mode: typert.CodecSrcJSON},
		}},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	gw := New(root, registry)
	result, err := gw.Invoke(context.Background(), InvokeRequest{
		Namespace: "scoped", Method: "Echo",
		Args: map[string]any{"agentId": "a-1", "text": "hi"},
	})
	if err != nil || result != "hello hi" {
		t.Fatalf("scoped invoke = %v, %v", result, err)
	}

	// Provider absent for an unknown context key.
	_, err = gw.Invoke(context.Background(), InvokeRequest{
		Namespace: "absent-scoped", Method: "Greet", Args: map[string]any{},
	})
	var gatewayErr *GatewayError
	if !errors.As(err, &gatewayErr) || gatewayErr.Code != CodeInvocationUnavailable {
		t.Fatalf("unregistered endpoint = %v", err)
	}

	// Not found.
	_, err = gw.Invoke(context.Background(), InvokeRequest{
		Namespace: "scoped", Method: "Echo", Args: map[string]any{"agentId": "missing", "text": "x"},
	})
	if !errors.As(err, &gatewayErr) || gatewayErr.Code != CodeContextNotFound {
		t.Fatalf("context not found = %v", err)
	}

	// Provider failure.
	registry.ContextGetHost("agent")
	failing := cordis.NewRoot(cordis.Discard{})
	defer func() { _ = failing.Dispose() }()
	failingRegistry := typert.NewRegistry(failing, cordis.Discard{})
	failing.Provide("demo", &demoService{})
	if _, err := failingRegistry.ContextRegisterHost("agent", typert.HostContextAdapter{
		Wire:           "agentId",
		WireTypeSymbol: "AgentId",
		Identity:       func(any) (any, bool) { return "a-1", true },
		Resolve:        func(any) (any, bool, error) { return nil, false, errors.New("store down") },
	}); err != nil {
		t.Fatalf("register failing host: %v", err)
	}
	if _, err := failingRegistry.Register(typert.Contribution{
		Package: "demo", Face: typert.FaceHost,
		Invocations: []typert.InvocationDescriptor{{
			ID: "scoped.greet", Service: "demo", Namespace: "scoped", Method: "Echo",
			Invocation: typert.InvocationReceiver{
				Kind: typert.ReceiverContext, Context: "agent", Wire: "agentId",
				Codec: typert.Codec{Mode: typert.CodecSrcJSON},
			},
			Result: typert.Codec{Mode: typert.CodecSrcJSON},
		}},
	}); err != nil {
		t.Fatalf("register failing contribution: %v", err)
	}
	_, err = New(failing, failingRegistry).Invoke(context.Background(), InvokeRequest{
		Namespace: "scoped", Method: "Echo", Args: map[string]any{"agentId": "a-1"},
	})
	if !errors.As(err, &gatewayErr) || gatewayErr.Code != CodeContextFailed {
		t.Fatalf("context failed = %v", err)
	}

	// Provider missing entirely.
	noProvider := cordis.NewRoot(cordis.Discard{})
	defer func() { _ = noProvider.Dispose() }()
	noProviderRegistry := typert.NewRegistry(noProvider, cordis.Discard{})
	noProvider.Provide("demo", &demoService{})
	if _, err := noProviderRegistry.Register(typert.Contribution{
		Package: "demo", Face: typert.FaceHost,
		Invocations: []typert.InvocationDescriptor{{
			ID: "scoped.greet", Service: "demo", Namespace: "scoped", Method: "Echo",
			Invocation: typert.InvocationReceiver{
				Kind: typert.ReceiverContext, Context: "agent", Wire: "agentId",
				Codec: typert.Codec{Mode: typert.CodecSrcJSON},
			},
			Result: typert.Codec{Mode: typert.CodecSrcJSON},
		}},
	}); err != nil {
		t.Fatalf("register no-provider contribution: %v", err)
	}
	_, err = New(noProvider, noProviderRegistry).Invoke(context.Background(), InvokeRequest{
		Namespace: "scoped", Method: "Echo", Args: map[string]any{"agentId": "a-1"},
	})
	if !errors.As(err, &gatewayErr) || gatewayErr.Code != CodeContextUnavailable {
		t.Fatalf("context unavailable = %v", err)
	}
}

// agentStub stands in for the AgentRegistry in the context tests.
type agentStub struct{ agents map[string]*cordis.Context }

func TestWireFailureMapping(t *testing.T) {
	cancelled := WireFailure(&invocationCancelled{endpoint: "a/b", cause: errors.New("late")})
	if cancelled.Code != "cancelled" {
		t.Fatalf("cancelled mapping = %+v", cancelled)
	}
	internal := WireFailure(errors.New("boom"))
	if internal.Code != "internal" || internal.Message != "boom" {
		t.Fatalf("internal mapping = %+v", internal)
	}
	policy := WireFailure(&typert.LookupFailure{Failure: typert.Failure{Code: "fenced"}})
	if policy.Code != "fenced" {
		t.Fatalf("policy mapping = %+v", policy)
	}
	business := WireFailure(&typert.RemoteFailure{Failure: typert.Failure{Code: "domain"}})
	if business.Code != "domain" {
		t.Fatalf("business mapping = %+v", business)
	}
}

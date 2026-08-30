package typert

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"dshgo/cordis"
)

func strictCodec(symbol string) Codec {
	return Codec{
		Mode:       CodecStrict,
		TypeSymbol: symbol,
		Validate: func(value []byte) error {
			var v any
			return json.Unmarshal(value, &v)
		},
	}
}

func validDescriptor(id string, namespace string, method string) InvocationDescriptor {
	return InvocationDescriptor{
		ID:        id,
		Service:   "myService",
		Namespace: namespace,
		Method:    method,
		Invocation: InvocationReceiver{
			Kind: ReceiverDirect,
		},
		Parameters: []InvocationParameterDescriptor{
			{Name: "text", Wire: "text", Source: SourceJSON, Codec: Codec{Mode: CodecSrcJSON}},
		},
		Result: strictCodec("MyResult"),
	}
}

func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	root := cordis.NewRoot(cordis.Discard{})
	registry := NewRegistry(root, cordis.Discard{})
	t.Cleanup(func() { _ = root.Dispose() })
	return registry
}

func TestKeyComposition(t *testing.T) {
	if got := TypertKey("@deepseek-ai/dsh-agent", "AgentInput"); got != "@deepseek-ai/dsh-agent#AgentInput" {
		t.Fatalf("TypertKey = %q", got)
	}
	if got := TypertPackageKey("pkg", FaceClient); got != "pkg#client" {
		t.Fatalf("TypertPackageKey = %q", got)
	}
	endpoint := TypertEndpoint(validDescriptor("id", "agents", "run"))
	if endpoint != "agents/run" {
		t.Fatalf("endpoint = %q", endpoint)
	}
}

func TestValidateInvocationAcceptsWellFormed(t *testing.T) {
	descriptor := validDescriptor("svc.run", "agents", "run")
	descriptor.Scope = &InvocationScope{Context: "agent", Wire: "text"}
	// Scope requires its own lookup parameter selection: build one.
	descriptor.Scope = nil
	descriptor.CancellationParameter = "signal"
	if err := ValidateInvocation(&descriptor); err != nil {
		t.Fatalf("well-formed descriptor rejected: %v", err)
	}
}

func TestValidateInvocationRejections(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(d *InvocationDescriptor)
		message string
	}{
		{"empty id", func(d *InvocationDescriptor) { d.ID = "" }, "must be nonempty"},
		{"service with #", func(d *InvocationDescriptor) { d.Service = "a#b" }, `must be nonempty and must not contain "#"`},
		{"bad namespace", func(d *InvocationDescriptor) { d.Namespace = "a b" }, "RPC endpoint segment characters"},
		{"strict result without symbol", func(d *InvocationDescriptor) {
			d.Result = Codec{Mode: CodecStrict}
		}, "type symbol"},
		{"strict result without validator", func(d *InvocationDescriptor) {
			d.Result = Codec{Mode: CodecStrict, TypeSymbol: "X"}
		}, "strict codec has no parse() method"},
		{"duplicate wire field", func(d *InvocationDescriptor) {
			d.Parameters = append(d.Parameters, InvocationParameterDescriptor{
				Name: "other", Wire: "text", Source: SourceJSON, Codec: Codec{Mode: CodecSrcJSON},
			})
		}, `repeats wire field "text"`},
		{"lookup param accepts undefined", func(d *InvocationDescriptor) {
			d.Parameters[0] = InvocationParameterDescriptor{
				Name: "s", Wire: "session", Source: SourceLookup, Lookup: "session",
				Codec: Codec{Mode: CodecSrcJSON}, AcceptsUndefined: true,
			}
		}, "cannot accept undefined"},
		{"lookup param missing key", func(d *InvocationDescriptor) {
			d.Parameters[0] = InvocationParameterDescriptor{
				Name: "s", Wire: "session", Source: SourceLookup,
				Codec: Codec{Mode: CodecSrcJSON},
			}
		}, "has no lookup key"},
		{"json param with lookup key", func(d *InvocationDescriptor) {
			d.Parameters[0].Lookup = "session"
		}, "declares a lookup key"},
		{"foreign cancellation parameter", func(d *InvocationDescriptor) {
			d.CancellationParameter = "abort"
		}, `must be "signal"`},
		{"scope on context receiver", func(d *InvocationDescriptor) {
			d.Invocation = InvocationReceiver{Kind: ReceiverContext, Context: "agent", Wire: "agentId", Codec: Codec{Mode: CodecSrcJSON}}
			d.Scope = &InvocationScope{Context: "agent", Wire: "session"}
		}, "cannot declare a direct scope projection"},
		{"context receiver repeats wire", func(d *InvocationDescriptor) {
			d.Invocation = InvocationReceiver{Kind: ReceiverContext, Context: "agent", Wire: "text", Codec: Codec{Mode: CodecSrcJSON}}
		}, `repeats wire field "text"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			descriptor := validDescriptor("svc.run", "agents", "run")
			tc.mutate(&descriptor)
			err := ValidateInvocation(&descriptor)
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("error = %v, want containing %q", err, tc.message)
			}
		})
	}
}

func TestValidateInvocationScopeMustSelectItsOnlyLookup(t *testing.T) {
	descriptor := validDescriptor("svc.run", "agents", "run")
	descriptor.Parameters = []InvocationParameterDescriptor{
		{Name: "agent", Wire: "agentRef", Source: SourceLookup, Lookup: "agent", Codec: Codec{Mode: CodecSrcJSON}},
	}
	descriptor.Scope = &InvocationScope{Context: "agent", Wire: "agentRef"}
	if err := ValidateInvocation(&descriptor); err != nil {
		t.Fatalf("scope over the only lookup parameter must pass: %v", err)
	}
	descriptor.Scope.Wire = "other"
	err := ValidateInvocation(&descriptor)
	if err == nil || !strings.Contains(err.Error(), "must select its only lookup parameter") {
		t.Fatalf("mismatched scope = %v", err)
	}
}

func TestContributionRegisterAndWithdrawRoundTrip(t *testing.T) {
	registry := newTestRegistry(t)
	var changes []RegistryChange
	done := make(chan struct{})
	registry.LocalSubscribe(func(change RegistryChange) {
		changes = append(changes, change)
		close(done)
	})
	disposer, err := registry.Register(Contribution{
		Package: "@deepseek-ai/dsh-demo",
		Face:    FaceHost,
		Schemas: []Schema{{Name: "Input", Validate: func([]byte) error { return nil }}},
		Invocations: []InvocationDescriptor{
			validDescriptor("demo.run", "demo", "run"),
		},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	<-done
	if _, ok := registry.Get("@deepseek-ai/dsh-demo#Input"); !ok {
		t.Fatal("schema missing after register")
	}
	if _, ok := registry.LocalGet("demo/run"); !ok {
		t.Fatal("invocation missing after register")
	}
	if !registry.LocalHasSeen("demo/run") {
		t.Fatal("history must survive withdrawal")
	}
	if got := len(registry.ListPackages(PackageFilter{Package: "@deepseek-ai/dsh-demo"})); got != 1 {
		t.Fatalf("packages = %d", got)
	}
	disposer()
	if _, ok := registry.Get("@deepseek-ai/dsh-demo#Input"); ok {
		t.Fatal("schema must withdraw")
	}
	if _, ok := registry.LocalGet("demo/run"); ok {
		t.Fatal("invocation must withdraw")
	}
	if !registry.LocalHasSeen("demo/run") {
		t.Fatal("hasSeen must stay true after withdrawal")
	}
}

func TestContributionWithdrawalUnwindsWithContextDispose(t *testing.T) {
	root := cordis.NewRoot(cordis.Discard{})
	registry := NewRegistry(root, cordis.Discard{})
	if _, err := registry.Register(Contribution{
		Package: "pkg", Face: FaceHost,
		Schemas: []Schema{{Name: "S", Validate: func([]byte) error { return nil }}},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := root.Dispose(); err != nil {
		t.Fatalf("dispose: %v", err)
	}
	if _, ok := registry.Get("pkg#S"); ok {
		t.Fatal("context disposal must withdraw the contribution")
	}
}

func TestResolveErrorVocabulary(t *testing.T) {
	registry := newTestRegistry(t)
	if _, err := registry.Resolve("malformed"); err == nil || !strings.Contains(err.Error(), `invalid schema key`) {
		t.Fatalf("malformed key = %v", err)
	}
	if _, err := registry.Register(Contribution{
		Package: "pkg", Face: FaceHost,
		Schemas: []Schema{{Name: "Other", Validate: func([]byte) error { return nil }}},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	_, err := registry.Resolve("pkg#Input")
	if err == nil || !strings.Contains(err.Error(), `is registered but contributes no schema named "Input"`) {
		t.Fatalf("registered package missing schema = %v", err)
	}
	_, err = registry.Resolve("absent#Input")
	if err == nil || !strings.Contains(err.Error(), `has no registered contribution`) {
		t.Fatalf("absent package = %v", err)
	}
}

func TestDuplicateRegistrationRejectedAtomically(t *testing.T) {
	registry := newTestRegistry(t)
	contribution := Contribution{
		Package: "pkg", Face: FaceHost,
		Schemas: []Schema{{Name: "S", Validate: func([]byte) error { return nil }}},
	}
	if _, err := registry.Register(contribution); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if _, err := registry.Register(contribution); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("duplicate package face = %v", err)
	}
	dupSchema := Contribution{
		Package: "other", Face: FaceHost,
		Schemas: []Schema{
			{Name: "A", Validate: func([]byte) error { return nil }},
			{Name: "A", Validate: func([]byte) error { return nil }},
		},
	}
	if _, err := registry.Register(dupSchema); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("duplicate schema in batch = %v", err)
	}
	// Atomicity: the batch rejected on the second schema must not have left
	// the first schema behind.
	if _, ok := registry.Get("other#A"); ok {
		t.Fatal("rejected batch must not leave partial registrations")
	}
}

func TestSchemaValidationWiresThrough(t *testing.T) {
	registry := newTestRegistry(t)
	if _, err := registry.Register(Contribution{
		Package: "pkg", Face: FaceHost,
		Schemas: []Schema{{Name: "S", Validate: func(value []byte) error {
			if !json.Valid(value) {
				return errors.New("bad json")
			}
			return nil
		}}},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	record, ok := registry.Get("pkg#S")
	if !ok {
		t.Fatal("schema missing")
	}
	if err := record.Validate([]byte("not json")); err == nil {
		t.Fatal("validator must run")
	}
}

func TestRemoteRegistrationLifecycle(t *testing.T) {
	registry := newTestRegistry(t)
	disposer, err := registry.RemoteRegister(TypertRemoteContribution{
		Package:     "@deepseek-ai/dsh-session-remote",
		Descriptors: []InvocationDescriptor{validDescriptor("remote.run", "remote", "run")},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := registry.RemoteRegister(TypertRemoteContribution{
		Package: "@deepseek-ai/dsh-session-remote",
	}); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("duplicate remote package = %v", err)
	}
	if _, err := registry.RemoteRegister(TypertRemoteContribution{
		Package:     "other",
		Descriptors: []InvocationDescriptor{validDescriptor("remote.run", "remote", "run")},
	}); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("endpoint conflict across packages = %v", err)
	}
	if _, ok := registry.RemoteGet("remote/run"); !ok {
		t.Fatal("remote descriptor missing")
	}
	disposer()
	if _, ok := registry.RemoteGet("remote/run"); ok {
		t.Fatal("remote descriptor must withdraw")
	}
	// The withdrawn endpoint may register again under another package.
	if _, err := registry.RemoteRegister(TypertRemoteContribution{
		Package:     "other",
		Descriptors: []InvocationDescriptor{validDescriptor("remote.run", "remote", "run")},
	}); err != nil {
		t.Fatalf("re-register after withdraw: %v", err)
	}
}

func TestLookupRegistrationAndConfigureOverride(t *testing.T) {
	registry := newTestRegistry(t)
	session := map[string]string{"s-1": "session-object"}
	disposer, err := registry.LookupRegister("session", LookupProvider{
		Parameter:      "session",
		Wire:           "sessionId",
		HostTypeSymbol: "@deepseek-ai/dsh-session#Session",
		WireTypeSymbol: "@deepseek-ai/dsh-session/types#SessionId",
		Resolve: func(id any) (any, error) {
			if value, ok := session[id.(string)]; ok {
				return value, nil
			}
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	provider, ok := registry.LookupGet("session")
	if !ok {
		t.Fatal("lookup missing")
	}
	resolved, err := provider.Resolve("s-1")
	if err != nil || resolved != "session-object" {
		t.Fatalf("resolve = %v, %v", resolved, err)
	}

	// A configuration replaces the resolution policy; disposal restores the
	// provider's default.
	configured, err := registry.LookupConfigure("session", func(any) (any, error) {
		return "configured", nil
	})
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	provider, _ = registry.LookupGet("session")
	if resolved, _ := provider.Resolve("s-1"); resolved != "configured" {
		t.Fatalf("override resolve = %v", resolved)
	}
	configured()
	provider, _ = registry.LookupGet("session")
	if resolved, _ := provider.Resolve("s-1"); resolved != "session-object" {
		t.Fatalf("default must restore, got %v", resolved)
	}
	disposer()

	// A resolver without a live provider stays unavailable.
	if _, err := registry.LookupConfigure("session", func(any) (any, error) { return "x", nil }); err != nil {
		t.Fatalf("configure without provider: %v", err)
	}
	if _, ok := registry.LookupGet("session"); ok {
		t.Fatal("resolver alone must stay unavailable")
	}
}

func TestLookupDefinitionDriftRefusedAfterWithdrawal(t *testing.T) {
	registry := newTestRegistry(t)
	disposer, err := registry.LookupRegister("session", LookupProvider{
		Parameter: "session", Wire: "sessionId",
		HostTypeSymbol: "H", WireTypeSymbol: "W",
		Resolve: func(any) (any, error) { return nil, nil },
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	disposer()
	_, err = registry.LookupRegister("session", LookupProvider{
		Parameter: "session", Wire: "renamed",
		HostTypeSymbol: "H", WireTypeSymbol: "W",
		Resolve: func(any) (any, error) { return nil, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "changed its wire declaration") {
		t.Fatalf("drift = %v", err)
	}
	// The identical declaration re-registers cleanly.
	if _, err := registry.LookupRegister("session", LookupProvider{
		Parameter: "session", Wire: "sessionId",
		HostTypeSymbol: "H", WireTypeSymbol: "W",
		Resolve: func(any) (any, error) { return nil, nil },
	}); err != nil {
		t.Fatalf("identical re-register: %v", err)
	}
	if got := registry.LookupDefinitions(); len(got) != 1 || got[0].Wire != "sessionId" {
		t.Fatalf("definitions = %+v", got)
	}
}

func TestLookupDuplicateRegisterRejected(t *testing.T) {
	registry := newTestRegistry(t)
	provider := LookupProvider{
		Parameter: "session", Wire: "sessionId",
		HostTypeSymbol: "H", WireTypeSymbol: "W",
		Resolve: func(any) (any, error) { return nil, nil },
	}
	if _, err := registry.LookupRegister("session", provider); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := registry.LookupRegister("session", provider); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("duplicate = %v", err)
	}
}

func TestHostContextAdapters(t *testing.T) {
	registry := newTestRegistry(t)
	ctxAgents := map[string]any{"a-1": "agent-context-1"}
	hostDisposer, err := registry.ContextRegisterHost("agent", HostContextAdapter{
		Wire: "agentId", WireTypeSymbol: "SessionId",
		Identity: func(ctx any) (any, bool) {
			if ctx == "agent-ctx" {
				return "a-1", true
			}
			return nil, false
		},
		Resolve: func(id any) (any, bool, error) {
			ctx, ok := ctxAgents[id.(string)]
			return ctx, ok, nil
		},
	})
	if err != nil {
		t.Fatalf("register host: %v", err)
	}
	identity, err := registry.ContextIdentifyHost("agent-ctx")
	if err != nil || identity.Kind != "agent" || identity.Identity != "a-1" {
		t.Fatalf("identify = %+v, %v", identity, err)
	}
	if identity, err := registry.ContextIdentifyHost("other"); err != nil || identity.Identity != nil {
		t.Fatalf("unrecognized context = %+v, %v", identity, err)
	}

	if _, err := registry.ContextRegisterHost("client", HostContextAdapter{
		Wire: "agentId", WireTypeSymbol: "SessionId",
		Identity: func(any) (any, bool) { return "a-1", true },
	}); err != nil {
		t.Fatalf("register second host: %v", err)
	}
	_, err = registry.ContextIdentifyHost("agent-ctx")
	if err == nil || !strings.Contains(err.Error(), `recognized by both "agent" and "client"`) {
		t.Fatalf("multi-match = %v", err)
	}

	if _, err := registry.ContextRegisterHost("agent", HostContextAdapter{}); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("duplicate host = %v", err)
	}

	// Host configure override composes through ContextGetHost.
	override, err := registry.ContextConfigureHost("agent", func(id any) (any, bool, error) {
		return "overridden", true, nil
	})
	if err != nil {
		t.Fatalf("configure host: %v", err)
	}
	adapter, _ := registry.ContextGetHost("agent")
	resolved, ok, err := adapter.Resolve("a-1")
	if err != nil || !ok || resolved != "overridden" {
		t.Fatalf("override resolve = %v, %v, %v", resolved, ok, err)
	}
	override()
	adapter, _ = registry.ContextGetHost("agent")
	if resolved, _, _ = adapter.Resolve("a-1"); resolved != "agent-context-1" {
		t.Fatalf("default must restore, got %v", resolved)
	}
	hostDisposer()
	if _, ok := registry.ContextGetHost("agent"); ok {
		t.Fatal("host adapter must withdraw")
	}

	// Client adapters register independently.
	if _, err := registry.ContextRegisterClient("agent", ClientContextAdapter{
		Identity: func(any) (any, bool) { return "c-1", true },
	}); err != nil {
		t.Fatalf("register client: %v", err)
	}
	if _, err := registry.ContextRegisterClient("agent", ClientContextAdapter{}); err == nil {
		t.Fatal("duplicate client adapter must reject")
	}
}

func TestObserverFailureContained(t *testing.T) {
	root := cordis.NewRoot(cordis.Discard{})
	logger := &captureLogger{}
	registry := NewRegistry(root, logger)
	var mu sync.Mutex
	survived := 0
	registry.LocalSubscribe(func(RegistryChange) {
		panic("observer explosion")
	})
	registry.LocalSubscribe(func(RegistryChange) {
		mu.Lock()
		survived++
		mu.Unlock()
	})
	disposer, err := registry.Register(Contribution{
		Package: "pkg", Face: FaceHost,
		Invocations: []InvocationDescriptor{validDescriptor("x", "x", "run")},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	disposer()
	mu.Lock()
	defer mu.Unlock()
	if survived != 2 {
		t.Fatalf("survived = %d (want 2)", survived)
	}
	if len(logger.warns) < 2 {
		t.Fatalf("observer failures must be reported, warns = %d", len(logger.warns))
	}
}

// captureLogger records warn surfaces for containment assertions.
type captureLogger struct {
	warns []string
}

func (l *captureLogger) Info(args ...any) {}
func (l *captureLogger) Warn(args ...any) {
	l.warns = append(l.warns, fmt.Sprint(args...))
}
func (l *captureLogger) Error(args ...any) {}

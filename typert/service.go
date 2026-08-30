package typert

import (
	"fmt"
	"sync"

	"dshgo/cordis"
)

// ContextService is the typed "typert" service handle.
var ContextService = cordis.DefineService[*Registry]("typert")

// Registry is the runtime registry of generated schemas, package reflection,
// local/Remote invocations, Host object lookups, and Host/Client Context
// adapters. It performs no TypeScript analysis or schema generation.
type Registry struct {
	ctx    *cordis.Context
	logger cordis.Logger
	report reporter

	schemas  registryMap[SchemaRecord]
	packages registryMap[PackageRecord]
	local    *descriptorStore
	remote   *remoteStore
	lookups  *lookupStore
	contexts *contextStore
}

// registryMap keeps insertion order for enumeration (the official Map's
// iteration order contract).
type registryMap[T any] struct {
	mu    sync.Mutex
	m     map[string]T
	order []string
}

func (m *registryMap[T]) set(key string, value T) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.m[key]; !exists {
		m.order = append(m.order, key)
	}
	m.m[key] = value
}

func (m *registryMap[T]) get(key string) (T, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	value, ok := m.m[key]
	return value, ok
}

func (m *registryMap[T]) delete(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.m, key)
	for i, existing := range m.order {
		if existing == key {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
}

// deleteIf removes the key only when it exists and the current value
// matches — the owner-guarded withdrawal path.
func (m *registryMap[T]) deleteIf(key string, match func(current T) bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.m[key]
	if !ok || !match(current) {
		return
	}
	delete(m.m, key)
	for i, existing := range m.order {
		if existing == key {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
}

func (m *registryMap[T]) snapshot() []T {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]T, 0, len(m.order))
	for _, key := range m.order {
		out = append(out, m.m[key])
	}
	return out
}

// NewRegistry builds the registry bound to one Cordis context: registration
// disposers unwind with that context's disposal, and observer failures warn
// through the given logger surface.
func NewRegistry(ctx *cordis.Context, logger cordis.Logger) *Registry {
	registry := &Registry{ctx: ctx, logger: logger}
	registry.report = func(change RegistryChange, err error) { registry.warn(change, err) }
	registry.schemas = registryMap[SchemaRecord]{m: map[string]SchemaRecord{}}
	registry.packages = registryMap[PackageRecord]{m: map[string]PackageRecord{}}
	registry.local = newDescriptorStore(ChangeLocal, registry.report)
	registry.remote = newRemoteStore(newDescriptorStore(ChangeRemote, registry.report))
	registry.lookups = newLookupStore(registry.report)
	registry.contexts = newContextStore(registry.report)
	return registry
}

// Register adds one generated contribution atomically: duplicate
// package-face identities, schemas, invocation ids, or endpoints reject the
// whole batch before any mutation. The returned disposer withdraws the
// contribution and also unwinds with the registry's context disposal;
// withdrawal is idempotent.
func (r *Registry) Register(contribution Contribution) (Disposer, error) {
	packageKey, packageRecord, err := r.validatePackage(contribution)
	if err != nil {
		return nil, err
	}
	schemaRecords, err := r.validateSchemas(contribution)
	if err != nil {
		return nil, err
	}
	owner := &ownerToken{}
	if err := r.local.install(owner, contribution.Invocations); err != nil {
		return nil, err
	}
	r.packages.set(packageKey, packageRecord)
	for _, record := range schemaRecords {
		r.schemas.set(record.Key, record)
	}
	withdraw := Disposer(func() {
		// Owner-guarded: a later registration of the same identity must
		// never be withdrawn by this disposer (double-withdraw is legal).
		r.packages.deleteIf(packageKey, func(current PackageRecord) bool { return current.Key == packageKey })
		for _, record := range schemaRecords {
			r.schemas.deleteIf(record.Key, func(current SchemaRecord) bool { return current.Key == record.Key })
		}
		r.local.withdraw(owner, contribution.Invocations)
	})
	if err := r.ctx.Effect(func() (cordis.Disposer, error) { return withdraw, nil }); err != nil {
		withdraw()
		return nil, err
	}
	return withdraw, nil
}

// Get looks up one schema by `<package>#<name>`.
func (r *Registry) Get(key string) (SchemaRecord, bool) {
	return r.schemas.get(key)
}

// Resolve resolves one required schema with the official error vocabulary:
// a malformed key, a registered package missing the named schema, and an
// unregistered package each name their case.
func (r *Registry) Resolve(key string) (SchemaRecord, error) {
	if record, ok := r.schemas.get(key); ok {
		return record, nil
	}
	hash := -1
	for i := 0; i < len(key); i++ {
		if key[i] == '#' {
			hash = i
			break
		}
	}
	if hash <= 0 || hash == len(key)-1 {
		return SchemaRecord{}, fmt.Errorf("typert: invalid schema key %q — expected %q", key, "<package>#<name>")
	}
	packageName := key[:hash]
	for _, record := range r.packages.snapshot() {
		if record.Package == packageName {
			return SchemaRecord{}, fmt.Errorf(
				"typert: cannot resolve %q — package %q is registered but contributes no schema named %q",
				key, packageName, key[hash+1:])
		}
	}
	return SchemaRecord{}, fmt.Errorf("typert: cannot resolve %q — package %q has no registered contribution", key, packageName)
}

// List enumerates live schemas in registration order.
func (r *Registry) List(filter SchemaFilter) []SchemaRecord {
	out := []SchemaRecord{}
	for _, record := range r.schemas.snapshot() {
		if matchesFace(record.Package, record.Face, filter.Package, filter.Face) {
			out = append(out, record)
		}
	}
	return out
}

// GetPackage looks up generated reflection for one package face.
func (r *Registry) GetPackage(packageName string, face Face) (PackageRecord, bool) {
	return r.packages.get(TypertPackageKey(packageName, face))
}

// ListPackages enumerates generated package reflection in registration
// order.
func (r *Registry) ListPackages(filter PackageFilter) []PackageRecord {
	out := []PackageRecord{}
	for _, record := range r.packages.snapshot() {
		if matchesFace(record.Package, record.Face, filter.Package, filter.Face) {
			out = append(out, record)
		}
	}
	return out
}

// LocalGet looks up one local invocation by `<namespace>/<method>`.
func (r *Registry) LocalGet(endpoint string) (InvocationDescriptor, bool) {
	return r.local.get(endpoint)
}

// LocalHasSeen reports whether a definition has existed during this
// registry's lifetime, even if withdrawn.
func (r *Registry) LocalHasSeen(endpoint string) bool { return r.local.hasSeen(endpoint) }

// LocalList snapshots the local descriptors in registration order.
func (r *Registry) LocalList() []InvocationDescriptor { return r.local.list() }

// LocalSubscribe observes later local-definition changes.
func (r *Registry) LocalSubscribe(listener RegistryListener) Disposer {
	return r.local.subscribe(listener)
}

// RemoteRegister mounts one consumer-selected Remote contribution.
func (r *Registry) RemoteRegister(contribution TypertRemoteContribution) (Disposer, error) {
	return r.remote.register(contribution)
}

// RemoteGet looks up one Remote descriptor by endpoint.
func (r *Registry) RemoteGet(endpoint string) (InvocationDescriptor, bool) {
	return r.remote.descriptors.get(endpoint)
}

// RemoteList snapshots the Remote descriptors in registration order.
func (r *Registry) RemoteList() []InvocationDescriptor { return r.remote.descriptors.list() }

// RemoteSubscribe observes later Remote contribution changes.
func (r *Registry) RemoteSubscribe(listener RegistryListener) Disposer {
	return r.remote.descriptors.subscribe(listener)
}

// LookupRegister registers one Host object lookup provider.
func (r *Registry) LookupRegister(key string, provider LookupProvider) (Disposer, error) {
	return r.lookups.register(key, provider)
}

// LookupConfigure replaces one provider's default resolution policy while
// the configuration is active; configuration may precede provider
// registration.
func (r *Registry) LookupConfigure(key string, resolver LookupResolver) (Disposer, error) {
	return r.lookups.configure(key, resolver)
}

// LookupGet returns the live provider with its configured resolver applied;
// a resolver without a live provider stays unavailable.
func (r *Registry) LookupGet(key string) (LookupProvider, bool) { return r.lookups.get(key) }

// LookupDefinitions returns the wire declarations observed during this
// registry's lifetime.
func (r *Registry) LookupDefinitions() []LookupDefinition {
	return r.lookups.definitionsSnapshot()
}

// LookupKeys snapshots the registered provider keys.
func (r *Registry) LookupKeys() []string { return r.lookups.keys() }

// LookupSubscribe observes later lookup changes.
func (r *Registry) LookupSubscribe(listener RegistryListener) Disposer {
	return r.lookups.changes.subscribe(listener)
}

// ContextRegisterHost registers a Host Context adapter.
func (r *Registry) ContextRegisterHost(key string, adapter HostContextAdapter) (Disposer, error) {
	return r.contexts.registerHost(key, adapter)
}

// ContextConfigureHost overrides one Host Context key's resolution policy
// for this registration; disposal restores the adapter's default resolver.
func (r *Registry) ContextConfigureHost(key string, resolver HostContextResolver) (Disposer, error) {
	return r.contexts.configureHost(key, resolver)
}

// ContextRegisterClient registers a Client Context adapter.
func (r *Registry) ContextRegisterClient(key string, adapter ClientContextAdapter) (Disposer, error) {
	return r.contexts.registerClient(key, adapter)
}

// ContextIdentifyHost identifies a live Host Context through the sole
// registered adapter set; two recognizers reject.
func (r *Registry) ContextIdentifyHost(ctx any) (HostContextIdentity, error) {
	return r.contexts.identifyHost(ctx)
}

// ContextGetHost looks up a Host Context adapter.
func (r *Registry) ContextGetHost(key string) (HostContextAdapter, bool) {
	return r.contexts.getHost(key)
}

// ContextGetClient looks up a Client Context adapter.
func (r *Registry) ContextGetClient(key string) (ClientContextAdapter, bool) {
	return r.contexts.getClient(key)
}

// ContextSubscribe observes later Context adapter changes.
func (r *Registry) ContextSubscribe(listener RegistryListener) Disposer {
	return r.contexts.changes.subscribe(listener)
}

func (r *Registry) validatePackage(contribution Contribution) (string, PackageRecord, error) {
	if err := validateSegment("package name", contribution.Package); err != nil {
		return "", PackageRecord{}, err
	}
	if contribution.Face != FaceHost && contribution.Face != FaceClient {
		return "", PackageRecord{}, fmt.Errorf("typert: invalid face %q — expected %q or %q", string(contribution.Face), string(FaceHost), string(FaceClient))
	}
	key := TypertPackageKey(contribution.Package, contribution.Face)
	if _, taken := r.packages.get(key); taken {
		return "", PackageRecord{}, fmt.Errorf("typert: package face %q is already registered", key)
	}
	return key, PackageRecord{
		Package: contribution.Package,
		Face:    contribution.Face,
		Key:     key,
		Model:   contribution.Model,
	}, nil
}

func (r *Registry) validateSchemas(contribution Contribution) ([]SchemaRecord, error) {
	records := []SchemaRecord{}
	batch := map[string]bool{}
	for _, schema := range contribution.Schemas {
		if err := validateSegment("schema name", schema.Name); err != nil {
			return nil, err
		}
		key := TypertKey(contribution.Package, schema.Name)
		if batch[key] {
			return nil, fmt.Errorf("typert: schema %q is already registered", key)
		}
		if _, taken := r.schemas.get(key); taken {
			return nil, fmt.Errorf("typert: schema %q is already registered", key)
		}
		batch[key] = true
		records = append(records, SchemaRecord{
			Schema:  schema,
			Package: contribution.Package,
			Face:    contribution.Face,
			Key:     key,
		})
	}
	return records, nil
}

func matchesFace(recordPackage string, recordFace Face, filterPackage string, filterFace Face) bool {
	return (filterPackage == "" || recordPackage == filterPackage) &&
		(filterFace == "" || recordFace == filterFace)
}

// warn reports one observer failure through the registry's logger, if any.
func (r *Registry) warn(change RegistryChange, err error) {
	if r.logger == nil {
		return
	}
	r.logger.Warn(fmt.Sprintf("typert: %s observer for %q failed", change.Kind, change.Key))
	r.logger.Warn(err)
}

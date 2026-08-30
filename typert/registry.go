package typert

import (
	"fmt"
	"sync"

	"dshgo/cordis"
)

// Schema carries one generated schema's validation entry point. The official
// store keeps live Zod schemas; the Go store keeps the equivalent validator
// closure (the toJSONSchema projection stays with the deferred generator
// story — see the package comment).
type Schema struct {
	// Name is the schema export name within the contributing package.
	Name string
	// Validate parses and validates one boundary value.
	Validate func(value []byte) error
}

// DocTag is one structured JSDoc tag retained by generated runtime metadata.
type DocTag struct {
	Name     string
	Argument string
	Comment  string
	Text     string
}

// Documentation is the source documentation retained on reflected package
// elements.
type Documentation struct {
	Description string
	Summary     string
	Tags        []DocTag
	JSDoc       string
}

// MemberModel is one generated public member signature.
type MemberModel struct {
	Kind      string
	Name      string
	Signature string
	Summary   string
	JSDoc     string
}

// TypeModel is one named type declaration referenced by a reflected business
// surface.
type TypeModel struct {
	Name        string
	Declaration string
}

// ServiceModel is the runtime reflection metadata for one Cordis service.
type ServiceModel struct {
	Documentation
	Key        string
	ExportName string
	Members    []MemberModel
	Types      []TypeModel
}

// EventModel is the runtime reflection metadata for one Cordis event.
type EventModel struct {
	Documentation
	Name      string
	Mode      string
	Signature string
}

// ObjectModel is the runtime reflection metadata for one explicitly exported
// reference object.
type ObjectModel struct {
	Documentation
	Name       string
	ExportName string
	Members    []MemberModel
	Types      []TypeModel
}

// PackageModel is the generated business reflection for one package on one
// face.
type PackageModel struct {
	Services []ServiceModel
	Events   []EventModel
	Objects  []ObjectModel
}

// Contribution is one generated package contribution registered and
// withdrawn atomically.
type Contribution struct {
	// Package is the contributing npm package.
	Package string
	// Face is the independently compiled side.
	Face Face
	// Schemas are the generated live validators.
	Schemas []Schema
	// Model is the generated package reflection.
	Model PackageModel
	// Invocations are the Host invocation definitions; empty when the
	// package exports no Remote methods.
	Invocations []InvocationDescriptor
}

// SchemaRecord is a live schema plus its contribution identity.
type SchemaRecord struct {
	Schema
	Package string
	Face    Face
	Key     string
}

// PackageRecord is a live generated package model plus its stable identity.
type PackageRecord struct {
	Package string
	Face    Face
	Key     string
	Model   PackageModel
}

// SchemaFilter filters schema enumeration.
type SchemaFilter struct {
	Package string
	Face    Face
}

// PackageFilter filters package-model enumeration.
type PackageFilter struct {
	Package string
	Face    Face
}

// Disposer withdraws one contribution or subscription.
type Disposer = cordis.Disposer

// ownerToken identifies the registration that owns store entries; duplicate
// registration is rejected, so an owner is always the unique owner of its
// entries.
type ownerToken struct{}

// reporter receives observer failures; the official registry warns through
// the Cordis logger.
type reporter func(change RegistryChange, err error)

// changeSource is the contained listener fan-out shared by every store.
// Listeners run in subscription order; failures and panics are reported and
// never starve the remaining listeners.
type changeSource struct {
	mu        sync.Mutex
	listeners []listenerEntry
	nextID    int
	report    reporter
}

type listenerEntry struct {
	id       int
	listener RegistryListener
}

func newChangeSource(report reporter) *changeSource {
	return &changeSource{report: report}
}

func (c *changeSource) subscribe(listener RegistryListener) Disposer {
	c.mu.Lock()
	c.nextID++
	entry := listenerEntry{id: c.nextID, listener: listener}
	c.listeners = append(c.listeners, entry)
	c.mu.Unlock()
	return Disposer(func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		for i, existing := range c.listeners {
			if existing.id == entry.id {
				c.listeners = append(c.listeners[:i], c.listeners[i+1:]...)
				break
			}
		}
	})
}

// emit snapshots the listener table outside any store lock, then runs every
// listener contained.
func (c *changeSource) emit(change RegistryChange) {
	c.mu.Lock()
	listeners := append([]listenerEntry(nil), c.listeners...)
	c.mu.Unlock()
	for _, entry := range listeners {
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					if c.report != nil {
						c.report(change, fmt.Errorf("%v", recovered))
					}
				}
			}()
			entry.listener(change)
		}()
	}
}

// descriptorStore tracks local or Remote invocation definitions: endpoint-
// and id-keyed, owner-tracked, with a seen-history that outlives withdrawal.
type descriptorStore struct {
	mu      sync.Mutex
	kind    ChangeKind
	entries map[string]InvocationDescriptor
	owners  map[string]*ownerToken
	ids     map[string]*ownerToken
	history map[string]bool
	order   []string
	changes *changeSource
}

func newDescriptorStore(kind ChangeKind, report reporter) *descriptorStore {
	return &descriptorStore{
		kind:    kind,
		entries: map[string]InvocationDescriptor{},
		owners:  map[string]*ownerToken{},
		ids:     map[string]*ownerToken{},
		history: map[string]bool{},
		changes: newChangeSource(report),
	}
}

// validateLocked refuses a batch that duplicates an endpoint or id inside
// the batch or against the live store. It mutates nothing. Callers hold mu.
func (s *descriptorStore) validateLocked(descriptors []InvocationDescriptor) error {
	endpoints := map[string]bool{}
	ids := map[string]bool{}
	for i := range descriptors {
		descriptor := &descriptors[i]
		if err := ValidateInvocation(descriptor); err != nil {
			return err
		}
		endpoint := TypertEndpoint(*descriptor)
		if endpoints[endpoint] {
			return fmt.Errorf("typert: %s endpoint %q is already registered", s.kind, endpoint)
		}
		if _, taken := s.entries[endpoint]; taken {
			return fmt.Errorf("typert: %s endpoint %q is already registered", s.kind, endpoint)
		}
		if ids[descriptor.ID] {
			return fmt.Errorf("typert: %s invocation id %q is already registered", s.kind, descriptor.ID)
		}
		if _, taken := s.ids[descriptor.ID]; taken {
			return fmt.Errorf("typert: %s invocation id %q is already registered", s.kind, descriptor.ID)
		}
		endpoints[endpoint] = true
		ids[descriptor.ID] = true
	}
	return nil
}

// install atomically validates and commits one batch under a single lock
// hold, then emits one change per descriptor outside the lock — a
// concurrent registration cannot win the same endpoint or id between the
// check and the install.
func (s *descriptorStore) install(owner *ownerToken, descriptors []InvocationDescriptor) error {
	keys := make([]string, 0, len(descriptors))
	s.mu.Lock()
	if err := s.validateLocked(descriptors); err != nil {
		s.mu.Unlock()
		return err
	}
	for i := range descriptors {
		endpoint := TypertEndpoint(descriptors[i])
		s.entries[endpoint] = descriptors[i]
		s.owners[endpoint] = owner
		s.ids[descriptors[i].ID] = owner
		if !s.history[endpoint] {
			s.history[endpoint] = true
		}
		s.order = append(s.order, endpoint)
		keys = append(keys, endpoint)
	}
	s.mu.Unlock()
	for _, endpoint := range keys {
		s.changes.emit(RegistryChange{Kind: s.kind, Key: endpoint})
	}
	return nil
}

// withdraw removes exactly the calling owner's entries and emits their
// changes.
func (s *descriptorStore) withdraw(owner *ownerToken, descriptors []InvocationDescriptor) {
	removed := []string{}
	s.mu.Lock()
	for i := range descriptors {
		endpoint := TypertEndpoint(descriptors[i])
		if s.owners[endpoint] != owner {
			continue
		}
		if _, live := s.entries[endpoint]; !live {
			continue
		}
		delete(s.entries, endpoint)
		delete(s.owners, endpoint)
		if s.ids[descriptors[i].ID] == owner {
			delete(s.ids, descriptors[i].ID)
		}
		for j, existing := range s.order {
			if existing == endpoint {
				s.order = append(s.order[:j], s.order[j+1:]...)
				break
			}
		}
		removed = append(removed, endpoint)
	}
	s.mu.Unlock()
	for _, endpoint := range removed {
		s.changes.emit(RegistryChange{Kind: s.kind, Key: endpoint})
	}
}

func (s *descriptorStore) get(endpoint string) (InvocationDescriptor, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	descriptor, ok := s.entries[endpoint]
	return descriptor, ok
}

func (s *descriptorStore) hasSeen(endpoint string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.history[endpoint]
}

func (s *descriptorStore) list() []InvocationDescriptor {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]InvocationDescriptor, 0, len(s.order))
	for _, endpoint := range s.order {
		out = append(out, s.entries[endpoint])
	}
	return out
}

func (s *descriptorStore) subscribe(listener RegistryListener) Disposer {
	return s.changes.subscribe(listener)
}

// remoteStore registers consumer-selected Remote packages: package-unique
// identities over a shared descriptor store.
type remoteStore struct {
	mu          sync.Mutex
	packages    map[string]*ownerToken
	descriptors *descriptorStore
}

func newRemoteStore(descriptors *descriptorStore) *remoteStore {
	return &remoteStore{packages: map[string]*ownerToken{}, descriptors: descriptors}
}

// register validates and installs one Remote contribution, returning the
// exact disposer that withdraws it. Duplicate package registration rejects.
// The package seat and the descriptor install happen as one unit: the seat
// is claimed under the package lock (so a concurrent registration of the
// same package sees it taken), and a descriptor failure rolls the seat back.
func (s *remoteStore) register(contribution TypertRemoteContribution) (Disposer, error) {
	if err := validateSegment("Remote package name", contribution.Package); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if _, taken := s.packages[contribution.Package]; taken {
		s.mu.Unlock()
		return nil, fmt.Errorf("typert: Remote package %q is already registered", contribution.Package)
	}
	owner := &ownerToken{}
	s.packages[contribution.Package] = owner
	s.mu.Unlock()
	if err := s.descriptors.install(owner, contribution.Descriptors); err != nil {
		s.mu.Lock()
		if s.packages[contribution.Package] == owner {
			delete(s.packages, contribution.Package)
		}
		s.mu.Unlock()
		return nil, err
	}
	return Disposer(func() {
		s.mu.Lock()
		if s.packages[contribution.Package] == owner {
			delete(s.packages, contribution.Package)
		}
		s.mu.Unlock()
		s.descriptors.withdraw(owner, contribution.Descriptors)
	}), nil
}

// lookupStore tracks Host object lookup providers, their configured
// resolvers, and the stable wire declarations observed during the registry
// lifetime.
type lookupStore struct {
	mu          sync.Mutex
	providers   map[string]LookupProvider
	resolvers   map[string]LookupResolver
	definitions map[string]LookupDefinition
	changes     *changeSource
}

func newLookupStore(report reporter) *lookupStore {
	return &lookupStore{
		providers:   map[string]LookupProvider{},
		resolvers:   map[string]LookupResolver{},
		definitions: map[string]LookupDefinition{},
		changes:     newChangeSource(report),
	}
}

// register installs one provider under its merge-declared key, refusing
// duplicates and wire-declaration drift against the lifetime's observed
// declarations.
func (s *lookupStore) register(key string, provider LookupProvider) (Disposer, error) {
	if err := validateSegment("lookup key", key); err != nil {
		return nil, err
	}
	if err := validateSegment("lookup parameter", provider.Parameter); err != nil {
		return nil, err
	}
	if err := validateWireName("lookup wire field", provider.Wire); err != nil {
		return nil, err
	}
	if err := validateNonempty("lookup Host type symbol", provider.HostTypeSymbol); err != nil {
		return nil, err
	}
	if err := validateNonempty("lookup wire type symbol", provider.WireTypeSymbol); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if _, taken := s.providers[key]; taken {
		s.mu.Unlock()
		return nil, fmt.Errorf("typert: lookup %q is already registered", key)
	}
	definition := LookupDefinition{
		Key:            key,
		Parameter:      provider.Parameter,
		Wire:           provider.Wire,
		HostTypeSymbol: provider.HostTypeSymbol,
		WireTypeSymbol: provider.WireTypeSymbol,
	}
	if known, seen := s.definitions[key]; seen && !lookupDefinitionEquals(known, definition) {
		s.mu.Unlock()
		return nil, fmt.Errorf("typert: lookup %q changed its wire declaration during this registry lifetime", key)
	}
	s.providers[key] = provider
	s.definitions[key] = definition
	s.mu.Unlock()
	s.changes.emit(RegistryChange{Kind: ChangeLookup, Key: key})
	return Disposer(func() {
		s.mu.Lock()
		if _, live := s.providers[key]; live {
			delete(s.providers, key)
			s.mu.Unlock()
			s.changes.emit(RegistryChange{Kind: ChangeLookup, Key: key})
			return
		}
		s.mu.Unlock()
	}), nil
}

// configure replaces one key's resolution policy; configuration may precede
// provider registration. Disposal restores the provider's default resolver.
func (s *lookupStore) configure(key string, resolver LookupResolver) (Disposer, error) {
	if err := validateSegment("lookup key", key); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if _, taken := s.resolvers[key]; taken {
		s.mu.Unlock()
		return nil, fmt.Errorf("typert: lookup %q resolver is already configured", key)
	}
	s.resolvers[key] = resolver
	s.mu.Unlock()
	s.changes.emit(RegistryChange{Kind: ChangeLookup, Key: key})
	return Disposer(func() {
		s.mu.Lock()
		if resolverPointer(s.resolvers[key]) == resolverPointer(resolver) {
			delete(s.resolvers, key)
			s.mu.Unlock()
			s.changes.emit(RegistryChange{Kind: ChangeLookup, Key: key})
			return
		}
		s.mu.Unlock()
	}), nil
}

// get returns the live provider with its configured resolver applied, or
// absent when no provider is live (a resolver alone stays unavailable).
func (s *lookupStore) get(key string) (LookupProvider, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	provider, ok := s.providers[key]
	if !ok {
		return LookupProvider{}, false
	}
	if resolver, configured := s.resolvers[key]; configured {
		provider.Resolve = resolver
	}
	return provider, true
}

func (s *lookupStore) definitionsSnapshot() []LookupDefinition {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]LookupDefinition, 0, len(s.definitions))
	for _, definition := range s.definitions {
		out = append(out, definition)
	}
	return out
}

func (s *lookupStore) keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.providers))
	for key := range s.providers {
		out = append(out, key)
	}
	return out
}

func lookupDefinitionEquals(left, right LookupDefinition) bool {
	return left.Parameter == right.Parameter && left.Wire == right.Wire &&
		left.HostTypeSymbol == right.HostTypeSymbol && left.WireTypeSymbol == right.WireTypeSymbol
}

// resolverPointer renders a resolver's code pointer for identity comparison
// (Go function values compare only to nil).
func resolverPointer(r LookupResolver) string {
	return fmt.Sprintf("%p", r)
}

package typert

import (
	"fmt"
	"sync"
)

// contextStore tracks the Host and Client Context adapters of each Context
// kind, plus per-key Host resolution overrides.
type contextStore struct {
	mu            sync.Mutex
	hosts         map[string]HostContextAdapter
	hostResolvers map[string]HostContextResolver
	clients       map[string]ClientContextAdapter
	hostOrder     []string
	changes       *changeSource
}

func newContextStore(report reporter) *contextStore {
	return &contextStore{
		hosts:         map[string]HostContextAdapter{},
		hostResolvers: map[string]HostContextResolver{},
		clients:       map[string]ClientContextAdapter{},
		changes:       newChangeSource(report),
	}
}

// registerHost installs a Host Context adapter; duplicate keys reject.
func (s *contextStore) registerHost(key string, adapter HostContextAdapter) (Disposer, error) {
	if err := validateSegment("Context key", key); err != nil {
		return nil, err
	}
	// Duplicate identity is refused before the adapter's own shape is
	// inspected (the official store's first check).
	if err := s.ensureAbsent(ChangeHostContext, key); err != nil {
		return nil, err
	}
	if err := validateWireName("Context wire field", adapter.Wire); err != nil {
		return nil, err
	}
	if err := validateNonempty("Context wire type symbol", adapter.WireTypeSymbol); err != nil {
		return nil, err
	}
	return s.registerProvider(ChangeHostContext, key, adapter)
}

// ensureAbsent refuses an already-registered provider key without touching
// the incoming adapter.
func (s *contextStore) ensureAbsent(kind ChangeKind, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if kind == ChangeClientContex {
		if _, taken := s.clients[key]; taken {
			return fmt.Errorf("typert: client-context provider %q is already registered", key)
		}
		return nil
	}
	if _, taken := s.hosts[key]; taken {
		return fmt.Errorf("typert: %s provider %q is already registered", kind, key)
	}
	return nil
}

// registerClient installs a Client Context adapter; duplicate keys reject.
func (s *contextStore) registerClient(key string, adapter ClientContextAdapter) (Disposer, error) {
	if err := validateSegment("Context key", key); err != nil {
		return nil, err
	}
	return s.registerClientProvider(key, adapter)
}

// configureHost overrides one Host Context key's resolution policy;
// configuration may precede provider registration and disposal restores the
// adapter's default resolver.
func (s *contextStore) configureHost(key string, resolver HostContextResolver) (Disposer, error) {
	if err := validateSegment("Context key", key); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if _, taken := s.hostResolvers[key]; taken {
		s.mu.Unlock()
		return nil, fmt.Errorf("typert: host-context %q resolver is already configured", key)
	}
	s.hostResolvers[key] = resolver
	s.mu.Unlock()
	s.changes.emit(RegistryChange{Kind: ChangeHostContext, Key: key})
	return Disposer(func() {
		s.mu.Lock()
		if _, live := s.hostResolvers[key]; live && s.hostResolvers[key] != nil {
			// Pointer-compare through the map; only the configuring owner
			// removes its own override.
			if sameResolver(s.hostResolvers[key], resolver) {
				delete(s.hostResolvers, key)
				s.mu.Unlock()
				s.changes.emit(RegistryChange{Kind: ChangeHostContext, Key: key})
				return
			}
		}
		s.mu.Unlock()
	}), nil
}

func sameResolver(a, b HostContextResolver) bool {
	return fmt.Sprintf("%p", a) == fmt.Sprintf("%p", b)
}

// registerProvider is the shared Host install path with resolver
// composition.
func (s *contextStore) registerProvider(kind ChangeKind, key string, adapter HostContextAdapter) (Disposer, error) {
	s.mu.Lock()
	if _, taken := s.hosts[key]; taken {
		s.mu.Unlock()
		return nil, fmt.Errorf("typert: %s provider %q is already registered", kind, key)
	}
	s.hosts[key] = adapter
	s.hostOrder = append(s.hostOrder, key)
	s.mu.Unlock()
	s.changes.emit(RegistryChange{Kind: kind, Key: key})
	return Disposer(func() {
		s.mu.Lock()
		if _, live := s.hosts[key]; live {
			delete(s.hosts, key)
			for i, existing := range s.hostOrder {
				if existing == key {
					s.hostOrder = append(s.hostOrder[:i], s.hostOrder[i+1:]...)
					break
				}
			}
			s.mu.Unlock()
			s.changes.emit(RegistryChange{Kind: kind, Key: key})
			return
		}
		s.mu.Unlock()
	}), nil
}

func (s *contextStore) registerClientProvider(key string, adapter ClientContextAdapter) (Disposer, error) {
	s.mu.Lock()
	if _, taken := s.clients[key]; taken {
		s.mu.Unlock()
		return nil, fmt.Errorf("typert: client-context provider %q is already registered", key)
	}
	s.clients[key] = adapter
	s.mu.Unlock()
	s.changes.emit(RegistryChange{Kind: ChangeClientContex, Key: key})
	return Disposer(func() {
		s.mu.Lock()
		if _, live := s.clients[key]; live {
			delete(s.clients, key)
			s.mu.Unlock()
			s.changes.emit(RegistryChange{Kind: ChangeClientContex, Key: key})
			return
		}
		s.mu.Unlock()
	}), nil
}

// getHost returns the live Host adapter with its configured resolver
// applied, or absent when no adapter is registered.
func (s *contextStore) getHost(key string) (HostContextAdapter, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	adapter, ok := s.hosts[key]
	if !ok {
		return HostContextAdapter{}, false
	}
	if resolver, configured := s.hostResolvers[key]; configured {
		base := adapter.Resolve
		adapter.Resolve = func(id any) (any, bool, error) {
			return resolver(id)
		}
		_ = base
	}
	return adapter, true
}

// identifyHost selects the sole registered adapter recognizing the Context;
// two recognizers for one Context is a composition bug.
func (s *contextStore) identifyHost(ctx any) (HostContextIdentity, error) {
	s.mu.Lock()
	order := append([]string(nil), s.hostOrder...)
	hosts := make(map[string]HostContextAdapter, len(s.hosts))
	for key, adapter := range s.hosts {
		hosts[key] = adapter
	}
	s.mu.Unlock()
	var match *HostContextIdentity
	var matchKind string
	for _, key := range order {
		adapter := hosts[key]
		id, ok := adapter.Identity(ctx)
		if !ok {
			continue
		}
		if match != nil {
			return HostContextIdentity{}, fmt.Errorf(
				"typert: Host Context is recognized by both %q and %q", matchKind, key)
		}
		match = &HostContextIdentity{Kind: key, Identity: id}
		matchKind = key
	}
	if match == nil {
		return HostContextIdentity{}, nil
	}
	return *match, nil
}

func (s *contextStore) getClient(key string) (ClientContextAdapter, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	adapter, ok := s.clients[key]
	return adapter, ok
}

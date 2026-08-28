package storage

import (
	"encoding/json"
	"errors"
	"testing"

	"dshgo/storagedomain"
	"dshgo/storagejson"
)

func TestBackendRegistryLifecycle(t *testing.T) {
	registry := NewBackendRegistry()
	backend := storagejson.NewJsonStorageBackend(t.TempDir())
	t.Cleanup(func() { _ = backend.Close() })

	dispose, err := registry.Register("json", backend)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := registry.Register("json", backend); err == nil {
		t.Fatal("duplicate registration must reject")
	} else {
		var storageErr *Error
		if !errors.As(err, &storageErr) || storageErr.Code != CodeDuplicateBackend ||
			storageErr.Message != "storage backend 'json' is already registered" {
			t.Fatalf("err = %v, want the verbatim duplicate-backend error", err)
		}
	}
	if resolved, err := registry.Get("json"); err != nil || resolved != backend {
		t.Fatalf("get = %v %v", resolved, err)
	}

	// Dispose removes; a stale disposer firing again must not remove the
	// successor.
	dispose()
	if _, err := registry.Get("json"); err == nil {
		t.Fatal("the disposed name must be gone")
	} else {
		var storageErr *Error
		if !errors.As(err, &storageErr) || storageErr.Code != CodeBackendNotFound ||
			storageErr.Message != "storage backend 'json' is not registered (registered: none)" {
			t.Fatalf("err = %v, want the verbatim backend-not-found error", err)
		}
	}
	// The successor is a distinct backend instance: the stale disposer's
	// identity guard must not remove it.
	successor := storagejson.NewJsonStorageBackend(t.TempDir())
	t.Cleanup(func() { _ = successor.Close() })
	dispose2, err := registry.Register("json", successor)
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	dispose()
	if resolved, err := registry.Get("json"); err != nil || resolved != successor {
		t.Fatalf("stale disposer removed the successor: %v", err)
	}
	dispose2()
	if names := registry.Names(); len(names) != 0 {
		t.Fatalf("names = %v, want empty", names)
	}
}

func TestBackendRegistryUnknownListsRegistered(t *testing.T) {
	registry := NewBackendRegistry()
	if _, err := registry.Get("sqlite"); err == nil {
		t.Fatal("unknown name must reject")
	} else if err.Error() != "storage backend 'sqlite' is not registered (registered: none)" {
		t.Fatalf("err = %v", err)
	}
	backend := storagejson.NewJsonStorageBackend(t.TempDir())
	t.Cleanup(func() { _ = backend.Close() })
	if _, err := registry.Register("json", backend); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := registry.Get("sqlite"); err == nil ||
		err.Error() != "storage backend 'sqlite' is not registered (registered: json)" {
		t.Fatalf("err = %v", err)
	}
}

func TestMountForms(t *testing.T) {
	hub := NewHub()
	unmount, err := hub.Mount("domain", &stubFacility{})
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	if _, err := hub.Mount("domain", &stubFacility{}); err == nil {
		t.Fatal("duplicate mount must reject")
	} else {
		var storageErr *Error
		if !errors.As(err, &storageErr) || storageErr.Code != CodeDuplicateMount ||
			storageErr.Message != "storage form 'domain' is already mounted" {
			t.Fatalf("err = %v, want the verbatim duplicate-mount error", err)
		}
	}
	if _, err := hub.Form("domain"); err != nil {
		t.Fatalf("form: %v", err)
	}
	// Stale unmount guard.
	unmount()
	unmount2, err := hub.Mount("domain", &stubFacility{})
	if err != nil {
		t.Fatalf("re-mount: %v", err)
	}
	unmount()
	if _, err := hub.Form("domain"); err != nil {
		t.Fatalf("stale unmount removed the successor: %v", err)
	}
	unmount2()
	if _, err := hub.Form("domain"); err == nil {
		t.Fatal("unmounted form must reject")
	} else {
		var storageErr *Error
		if !errors.As(err, &storageErr) || storageErr.Code != CodeFormNotMounted ||
			storageErr.Message != "storage form 'domain' is not mounted" {
			t.Fatalf("err = %v, want the verbatim form-not-mounted error", err)
		}
	}
}

type stubFacility struct{ storagedomain.Facility }

func TestDomainAccessor(t *testing.T) {
	hub := NewHub()
	if _, err := hub.Domain(); err == nil {
		t.Fatal("an unmounted domain must reject")
	}
	root := t.TempDir()
	backend := storagejson.NewJsonStorageBackend(root)
	t.Cleanup(func() { _ = backend.Close() })
	disposeBackend, err := hub.Backend.Register("json", backend)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(disposeBackend)

	resolved, err := hub.Backend.Get("json")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	spec, err := storagedomain.DefineDomain(storagedomain.DomainSpec{
		Name: "notes", Version: 1, Layout: storagedomain.LayoutSingle,
		Tables: []string{"entries"}, HasGlobal: false,
	})
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	facility := storagedomain.NewFacility(storagedomain.Config{Backend: "json"}, map[string]storagedomain.Backend{"json": resolved}, nil)
	unmount, err := hub.Mount("domain", facility)
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	t.Cleanup(unmount)

	domain, err := hub.Domain()
	if err != nil {
		t.Fatalf("domain accessor: %v", err)
	}
	opened, err := domain.Open(spec)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := opened.Table("entries").Put("a", json.RawMessage(`{"text":"hi"}`)); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestStorageBackendServiceKey(t *testing.T) {
	if got := StorageBackendServiceKey("json"); got != "storage.backend.json" {
		t.Fatalf("key = %s", got)
	}
}

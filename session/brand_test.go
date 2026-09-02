package session

import (
	"reflect"
	"testing"
)

func TestNewSessionSeqRejectsNegative(t *testing.T) {
	if _, err := NewSessionSeq(-1); err == nil {
		t.Fatal("NewSessionSeq(-1) must reject a negative value")
	}
	if _, err := NewSessionSeq(-0); err != nil {
		t.Fatalf("NewSessionSeq(-0) must admit zero: %v", err)
	}
	if _, err := NewSessionSeq(0); err != nil {
		t.Fatalf("NewSessionSeq(0) must admit the first log position: %v", err)
	}
	if seq, err := NewSessionSeq(42); err != nil || seq != 42 {
		t.Fatalf("NewSessionSeq(42) = %d, %v", seq, err)
	}
}

func TestNewSessionLogOffsetRejectsNegative(t *testing.T) {
	if _, err := NewSessionLogOffset(-1); err == nil {
		t.Fatal("NewSessionLogOffset(-1) must reject a negative value")
	}
	if _, err := NewSessionLogOffset(0); err != nil {
		t.Fatalf("NewSessionLogOffset(0) must admit zero: %v", err)
	}
	if off, err := NewSessionLogOffset(7); err != nil || off != 7 {
		t.Fatalf("NewSessionLogOffset(7) = %d, %v", off, err)
	}
}

func TestSessionSeqAndOffsetAreDistinctBrands(t *testing.T) {
	seq := SessionSeq(3)
	offset := SessionLogOffset(3)
	// The brands are distinct types: assignment across them must not compile.
	if reflect.TypeOf(seq) == reflect.TypeOf(offset) {
		t.Fatal("SessionSeq and SessionLogOffset must be distinct types")
	}
}

func TestSessionHeaderSeededValidation(t *testing.T) {
	if err := validateSessionHeader("h1", SessionHeader{
		Version: SESSION_FORMAT_VERSION, ID: "h1", CreatedAt: 1, IsSeeded: false,
	}); err != nil {
		t.Fatalf("unseeded zero-cut header rejected: %v", err)
	}
	err := validateSessionHeader("h2", SessionHeader{
		Version: SESSION_FORMAT_VERSION, ID: "h2", CreatedAt: 1, IsSeeded: false,
		InheritedEventCount: 2,
	})
	if err == nil {
		t.Fatal("unseeded header with non-zero inheritedEventCount must be rejected")
	}
	if err := validateSessionHeader("h3", SessionHeader{
		Version: SESSION_FORMAT_VERSION, ID: "h3", CreatedAt: 1, IsSeeded: true,
		InheritedEventCount: 2,
	}); err != nil {
		t.Fatalf("seeded header with a cut rejected: %v", err)
	}
}

package messagefeedback

import (
	"encoding/json"
	"strings"
	"testing"

	"dshgo/session"
)

func TestValidateRowAcceptsAndRejects(t *testing.T) {
	good := `{"session":{"createdAt":100},"items":[{"messageId":"m1","rating":"positive","version":"v1","createdAt":100,"updatedAt":110}]}`
	if err := validateRow(json.RawMessage(good)); err != nil {
		t.Fatalf("good row rejected: %v", err)
	}
	cases := []struct {
		name string
		row  string
	}{
		{"duplicate messageId", `{"session":{"createdAt":1},"items":[{"messageId":"m","rating":"positive","version":"a","createdAt":1,"updatedAt":1},{"messageId":"m","rating":"negative","version":"b","createdAt":1,"updatedAt":1}]}`},
		{"duplicate version", `{"session":{"createdAt":1},"items":[{"messageId":"a","rating":"positive","version":"v","createdAt":1,"updatedAt":1},{"messageId":"b","rating":"negative","version":"v","createdAt":1,"updatedAt":1}]}`},
		{"bad rating", `{"session":{"createdAt":1},"items":[{"messageId":"m","rating":"meh","version":"v","createdAt":1,"updatedAt":1}]}`},
		{"blank note", `{"session":{"createdAt":1},"items":[{"messageId":"m","rating":"positive","note":"  ","version":"v","createdAt":1,"updatedAt":1}]}`},
		{"reversed timestamps", `{"session":{"createdAt":1},"items":[{"messageId":"m","rating":"positive","version":"v","createdAt":5,"updatedAt":1}]}`},
		{"unknown field", `{"session":{"createdAt":1},"items":[{"messageId":"m","rating":"positive","version":"v","createdAt":1,"updatedAt":1,"extra":1}]}`},
	}
	for _, tc := range cases {
		if err := validateRow(json.RawMessage(tc.row)); err == nil {
			t.Fatalf("%s: rejected row accepted", tc.name)
		}
	}
}

func TestResolveNoteBounds(t *testing.T) {
	service, err := New(Config{MaxNoteBytes: 5})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if result := service.resolveNote(nil); !result.OK {
		t.Fatalf("absent note must succeed")
	}
	blank := "   "
	if result := service.resolveNote(&blank); result.OK || result.Error == nil || result.Error.Code != FailureNoteBlank {
		t.Fatalf("blank note = %+v", result)
	}
	long := "abcdef"
	if result := service.resolveNote(&long); result.OK || result.Error == nil || result.Error.Code != FailureNoteTooLarge {
		t.Fatalf("long note = %+v", result)
	}
	good := "ab"
	if result := service.resolveNote(&good); !result.OK || result.Value.(*string) != &good {
		t.Fatalf("good note = %+v", result)
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	if _, err := New(Config{MaxNoteBytes: 0}); err == nil || !strings.Contains(err.Error(), "positive safe integer") {
		t.Fatalf("zero maxNoteBytes = %v", err)
	}
}

func TestVersionConflictCarriesCurrent(t *testing.T) {
	current := Item{MessageID: "m", Rating: RatingPositive, Version: "v9"}
	failure := (&Service{}).versionConflict(&current)
	if failure.Code != FailureVersionConflict || failure.Current == nil || failure.Current.Version != "v9" {
		t.Fatalf("version conflict = %+v", failure)
	}
}

func TestSameIdentityLifecycleFencing(t *testing.T) {
	header := makeTestHeader()
	identity := identityOf(header)
	if !sameIdentity(identity, header) {
		t.Fatal("same lifecycle must match")
	}
	other := header
	other.CreatedAt = header.CreatedAt + 1
	if sameIdentity(identity, other) {
		t.Fatal("reused id with different createdAt must be a different lifecycle")
	}
	if !sameHeaderIdentity(header, header) {
		t.Fatal("same header must match")
	}
	changed := header
	changed.CWD = "/other"
	if sameHeaderIdentity(header, changed) {
		t.Fatal("different cwd must differ")
	}
}

// makeTestHeader builds a minimal persisted session header.
func makeTestHeader() session.SessionHeader {
	return session.SessionHeader{ID: session.SessionID("s1"), CreatedAt: 100, CWD: "/ws"}
}

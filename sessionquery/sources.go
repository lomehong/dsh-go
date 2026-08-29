package sessionquery

import (
	session "dshgo/session"
)

// AssertSessionHeadersCompatible rejects incompatible observations of one
// logical session source: the immutable identity fields must agree exactly.
func AssertSessionHeadersCompatible(a, b session.SessionHeader) error {
	if a.Version != b.Version ||
		a.ID != b.ID ||
		a.CreatedAt != b.CreatedAt ||
		a.CWD != b.CWD ||
		a.ParentSession != b.ParentSession ||
		pointerValue(a.SeedLength) != pointerValue(b.SeedLength) ||
		pointerValue(a.DelegationDepth) != pointerValue(b.DelegationDepth) {
		return queryError(CodeSourceConflict, "session source headers conflict for session %q", a.ID)
	}
	return nil
}

func pointerValue(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

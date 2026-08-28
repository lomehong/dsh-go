// Package subagent ports packages/subagent/subagent foundations: delegation
// depth accounting, the durable child descriptor, canonical final-output
// selection, one-shot run settlement, and the continuable activation setup
// registry. The SubagentService/continuation manager (index.ts,
// continuation.ts, child-agent.ts, list-children.ts) lands in a later round.
//
// Go adaptations: the cordis `ctx.subagents` service property is an explicit
// constructor (later round); merge-extensible TS unions become structs with a
// mode discriminant plus fail-closed parsing at the durable boundary; the
// AbortSignal request fields become context.Context; SubagentRun.result
// becomes Result() (SubagentResult, error) where a child-level failure is a
// VALUE (stop reason) and only an infrastructure fault is an error.
package subagent

import (
	"dshgo/llm"
)

// SubagentError is the typed failure for the subagent seam.
type SubagentError struct {
	err *llm.Error
}

func newSubagentError(message, code string, cause error) SubagentError {
	return SubagentError{err: llm.NewError(code, message, cause)}
}

// Error implements error.
func (e SubagentError) Error() string { return e.err.Error() }

// Code is the stable machine-routable code.
func (e SubagentError) Code() string { return e.err.Code() }

// Unwrap reaches the harness error chain.
func (e SubagentError) Unwrap() error { return e.err }

// Subagent error codes.
const (
	CodeActivationSetupRevoked       = "ACTIVATION_SETUP_REVOKED"
	CodeActivationSetupReleaseFailed = "ACTIVATION_SETUP_RELEASE_FAILED"
)

// asSubagentError walks the Unwrap chain looking for a SubagentError.
func asSubagentError(err error, target *SubagentError) bool {
	for err != nil {
		if subagentErr, ok := err.(SubagentError); ok {
			*target = subagentErr
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}

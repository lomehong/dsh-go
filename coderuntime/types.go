// Package coderuntime re-implements the code-execution capability seam of
// @deepseek-ai/dsh-code-runtime (packages/code-runtime/code-runtime, official
// tag dsh-v0.1.2-alpha.1): the vocabulary a caller hands a code runtime and
// the outcome contract it gets back. The seam ships no execution provider —
// backends own programs, budgets, abort, and substrate failure; consumers own
// tools and sessions. Runtimes know nothing about either.
package coderuntime

import (
	"context"

	"dshgo/cordis"
)

// CodeJSONValue is one lossless JSON value crossing the runtime boundary:
// null, bool, number, string, arrays, and objects of these. Binding arguments
// and completion values MUST be representable as this type; a lossy value
// fails the run as FailureInvalidOutput rather than corrupting it.
type CodeJSONValue = any

// CodeBindingFunction is one host-side function exposed to the program as an
// async callable. The runtime bridges calls to it (possibly across a
// serialization boundary), so args and the resolution value MUST be lossless
// JSON. A rejection of this function surfaces inside the program as a
// rejection of the corresponding call.
type CodeBindingFunction func(args CodeJSONValue) (CodeJSONValue, error)

// Failure kinds — orthogonal outcomes reported independently: a budget expiry
// is not an exception, an abort is not a timeout, and a substrate death is
// neither.
const (
	// FailureException: the program threw or failed to parse/transform.
	FailureException = "exception"
	// FailureTimeout: an implementation-owned budget expired; the message
	// says which.
	FailureTimeout = "timeout"
	// FailureAbort: the request context fired.
	FailureAbort = "abort"
	// FailureWorkerExit: the execution substrate died without settling.
	FailureWorkerExit = "worker-exit"
	// FailureInvalidOutput: the completion value was not lossless JSON.
	FailureInvalidOutput = "invalid-output"
	// FailureOutputLimit: the serialized outer logs/value/diagnostic exceeded
	// the configured cap.
	FailureOutputLimit = "output-limit"
)

// CodeBindingErrorClass is the program-visible typed rejection for one binding
// namespace. The runtime injects a real error constructor under Name; rejected
// member calls become its instances and expose the exact member name through
// MemberNameProperty. Both strings are runtime data rather than knowledge of a
// particular consumer such as PTC mode.
type CodeBindingErrorClass struct {
	// Name is the constructor global and the resulting error's name; the same
	// portable identifier rule as CodeBindingNamespace.Global applies.
	Name string
	// MemberNameProperty is the non-empty own property carrying the member
	// name. The portable exclusion set is ReservedErrorMembers plus dunder
	// forms; any other name is accepted everywhere.
	MemberNameProperty string
}

// CodeBindingNamespace is a named group of CodeBindingFunctions the runtime
// exposes to the program as one global object (e.g. `tools`). Function names
// are arbitrary strings — a runtime must treat names like `__proto__` or
// `constructor` as ordinary own properties (null-prototype construction),
// never as prototype collisions.
type CodeBindingNamespace struct {
	// Global is the identifier the program sees. It must match the
	// LANGUAGE-PORTABLE identifier subset and no target language's reserved
	// words, so the same namespace list works against every backend. Names in
	// ReservedBindingGlobals are refused by every backend.
	Global string
	// Functions are the callable members, keyed by the exact name the program
	// calls.
	Functions map[string]CodeBindingFunction
	// ErrorClass optionally declares the namespace's program-visible typed
	// rejection contract.
	ErrorClass *CodeBindingErrorClass
}

// CodeRunRequest is one run: the program source plus everything the runtime
// acts on. Per the explicit-over-implicit convention, defaulting (time
// budgets, output caps) is the implementation's validated config — a request
// carries no optional tuning knobs for a hidden default to fill in.
type CodeRunRequest struct {
	// Program is the source, in the runtime's Language. It runs as the body
	// of an async function: top-level await and return are available, and the
	// completion value becomes CodeRunResult.Value.
	Program string
	// Bindings are the host functions exposed to the program, one global
	// object per namespace.
	Bindings []CodeBindingNamespace
	// Signal aborts the run: the runtime stops the program (hard, even
	// mid-loop) and resolves with a FailureAbort result. In-flight binding
	// calls are the CALLER's to settle — the runtime only stops asking.
	Signal context.Context
}

// CodeRunFailure reports why a run failed; the message is human-readable
// detail suitable for feeding back to a model to self-correct.
type CodeRunFailure struct {
	// Kind is the failure class (one of the Failure* constants).
	Kind string
	// Message is the human-readable detail.
	Message string
}

// CodeRunResult is the outcome of one run. A failure is a FIELD on a resolved
// result, never a rejection of Run — reporting a failed program is the
// caller's job, not an error path.
type CodeRunResult struct {
	// Value is the program's completion value (its top-level return), when it
	// ran to completion and the value crossed the runtime's lossless-JSON
	// boundary. Invalid or over-limit completions fail the run instead of
	// substituting a rendered string; a failed or value-less run leaves this
	// absent.
	Value CodeJSONValue
	// Logs is the text the program emitted, in order, bounded only as part of
	// the outer result.
	Logs []string
	// Error is present iff the run failed.
	Error *CodeRunFailure
}

// CodeRuntime executes one model-written program against host async bindings.
// Implementations bridge structured-cloneable bindings, materialize each
// declared namespace rejection class, treat programs as hostile peers,
// isolate runs from one another, and terminate and await in-flight runs on
// Close.
type CodeRuntime interface {
	// Language is the source language Run expects Program to be written in,
	// as a lowercase identifier. Informational, not gating — a consumer that
	// generates language-specific presentation switches on it and fails loud
	// on a language it cannot present. Well-known values: "typescript" and
	// "python".
	Language() string
	// Isolation is the execution substrate, as a lowercase identifier.
	// Informational, not gating — a descriptor so deployments and diagnostics
	// can tell backends apart, not a security claim. Well-known values:
	// "worker-thread", "process", "container".
	Isolation() string
	// Run executes one program against the request's bindings and captures
	// what it emitted. Program, budget, abort, and substrate failures resolve
	// in the result's Error field; a non-nil returned error reports Service
	// Definition contract misuse only (malformed bindings).
	Run(request CodeRunRequest) (CodeRunResult, error)
	// Close terminates and awaits every in-flight run.
	Close() error
}

// ContextService is the typed "codeRuntime" service handle; the assertion for
// the registry lookup lives here instead of at every consumer.
var ContextService = cordis.DefineService[CodeRuntime]("codeRuntime")

// Package typert re-implements the runtime side of the official Typert
// packages (@deepseek-ai/dsh-typert-protocol and @deepseek-ai/dsh-typert-
// registry, official tag dsh-v0.1.2-alpha.1): the carrier-independent Remote
// invocation vocabulary, the runtime registry of local/Remote invocation
// definitions, Host object lookups, and Host/Client Context adapters, with
// the official validation and change-notification contracts.
//
// Honest degradations against the official tree: the schema store carries Go
// validator functions instead of live Zod schemas (no toJSONSchema
// projection — that stays with the deferred generator story); the
// build-time generator (TypeScript AST analysis) and the loader (runtime
// loading of generated .ts catalogs through node:module type stripping) have
// no Go counterpart and are recorded as such. The official merge-declared
// TypertLookupMap/TypertContextMap interfaces become string-keyed runtime
// tables with any-typed hosts and identities; type safety collapses to the
// registration boundary, matching the registry's own runtime contract.
package typert

import (
	"fmt"
	"regexp"
	"strings"
)

// Face names an independently compiled side that produced a contribution.
type Face string

// The two faces.
const (
	FaceHost   Face = "host"
	FaceClient Face = "client"
)

// CodecMode discriminates the parameter/result codec union.
type CodecMode string

// Codec modes: strict (validate the wire JSON against the declared type) and
// src-json (pass the source-level JSON through unchanged).
const (
	CodecStrict  CodecMode = "strict"
	CodecSrcJSON CodecMode = "src-json"
)

// Codec is attached to one invocation parameter or result. Strict codecs
// validate the wire JSON (the official live Zod schema's parse role); the
// TypeSymbol names the canonical type for diagnostics.
type Codec struct {
	Mode       CodecMode
	TypeSymbol string
	// Validate parses and validates one boundary value; nil on a strict
	// codec is the official "strict codec has no parse() method" failure.
	Validate func(value []byte) error
}

// ParameterSource classifies where an invocation parameter's value comes
// from.
type ParameterSource string

// Parameter sources: json (a wire args field) and lookup (a registered Host
// object lookup keyed by Parameter.Lookup).
const (
	SourceJSON   ParameterSource = "json"
	SourceLookup ParameterSource = "lookup"
)

// InvocationParameterDescriptor is one ordered business parameter in a
// Remote invocation.
type InvocationParameterDescriptor struct {
	// Name is the source-level parameter name.
	Name string
	// Wire is the required key in the wire args object.
	Wire string
	// Source tells JSON fields and Host-lookup parameters apart.
	Source ParameterSource
	// Lookup is the lookup key when Source is lookup.
	Lookup string
	// Codec is the boundary codec for the wire representation.
	Codec Codec
	// AcceptsUndefined marks an explicitly declared optional wire field;
	// only a JSON parameter may carry it.
	AcceptsUndefined bool
}

// InvocationSourceLocation is the source declaration position retained for
// diagnostics from generated definitions.
type InvocationSourceLocation struct {
	File   string
	Line   int
	Column int
}

// InvocationReceiverKind selects how the receiver of an invocation is bound.
type InvocationReceiverKind string

// Receiver kinds: direct (the service instance) and context (a Context
// selected by a wire identity).
const (
	ReceiverDirect  InvocationReceiverKind = "direct"
	ReceiverContext InvocationReceiverKind = "context"
)

// InvocationReceiver is the receiver selection: a direct service member or a
// Context receiver resolved through a registered Context adapter.
type InvocationReceiver struct {
	Kind    InvocationReceiverKind
	Context string
	Wire    string
	Codec   Codec
}

// InvocationScope is the optional consuming-Context projection for one
// direct lookup parameter: the Client Context adapter of Scope.Context
// supplies the identity that replaces Scope.Wire.
type InvocationScope struct {
	// Context is the Context kind whose Client adapter supplies the identity.
	Context string
	// Wire is the lookup parameter wire field replaced by that identity.
	Wire string
}

// InvocationDescriptor is the carrier-independent description of one
// exported method invocation.
type InvocationDescriptor struct {
	// ID is the globally stable generated identity.
	ID string
	// Service is the Cordis service key owning the method.
	Service string
	// Namespace is the wire namespace, defaulting to the service key at the
	// definition site.
	Namespace string
	// Method is the public instance method name.
	Method string
	// Implementation is the service member invoked when Method is an alias.
	Implementation string
	// Mode is empty for unary calls; "stream" calls validate and deliver
	// every yielded item.
	Mode string
	// Invocation is the receiver selection.
	Invocation InvocationReceiver
	// Scope is the optional consuming-Context projection.
	Scope *InvocationScope
	// Parameters are the ordered business parameters.
	Parameters []InvocationParameterDescriptor
	// CancellationParameter is empty for calls without transport
	// cancellation; otherwise it must be "signal" (the reserved final Host
	// method parameter).
	CancellationParameter string
	// Result is the codec for the unary result or each yielded stream item.
	Result Codec
	// Source is the declaration position, diagnostics only.
	Source *InvocationSourceLocation
}

// TypertRemoteContribution is a generated Host contract selected explicitly
// by a Client assembly.
type TypertRemoteContribution struct {
	// Package is the npm package that owns the Remote methods.
	Package string
	// Descriptors are the consumer-side invocation descriptors generated
	// from that package.
	Descriptors []InvocationDescriptor
}

// LookupResolver resolves one validated wire identity to its Host object.
// (nil, nil) is the official `undefined` — the object is unavailable.
type LookupResolver func(id any) (any, error)

// LookupProvider is the runtime provider for one declared Host object
// lookup.
type LookupProvider struct {
	// Parameter is the source parameter name recognized by the SRC weak
	// parser.
	Parameter string
	// Wire is the wire field replacing the Host object parameter.
	Wire string
	// HostTypeSymbol is the canonical Host type symbol used by strict
	// generation.
	HostTypeSymbol string
	// WireTypeSymbol is the canonical wire type symbol used by strict
	// generation.
	WireTypeSymbol string
	// Resolve maps a wire identity through the provider's default policy.
	Resolve LookupResolver
}

// LookupDefinition is the stable wire declaration retained after a lookup
// provider unloads.
type LookupDefinition struct {
	Key            string
	Parameter      string
	Wire           string
	HostTypeSymbol string
	WireTypeSymbol string
}

// HostContextAdapter is a Host Context adapter plus the wire declaration
// used by strict Remote methods.
type HostContextAdapter struct {
	// Wire is the wire field carrying the Context identity.
	Wire string
	// WireTypeSymbol is the canonical wire type symbol used by strict
	// generation.
	WireTypeSymbol string
	// Identity reads the identity represented by a live Context; ok is
	// false when the Context has another kind.
	Identity func(ctx any) (id any, ok bool)
	// Resolve maps a wire identity to the live Context; ok is false when it
	// is unavailable.
	Resolve func(id any) (ctx any, ok bool, err error)
}

// HostContextResolver is a composition-owned resolver replacing one Host
// Context adapter's default lookup policy.
type HostContextResolver func(id any) (ctx any, ok bool, err error)

// ClientContextAdapter is the client-side bidirectional Context adapter.
type ClientContextAdapter struct {
	// Identity reads the identity represented by a live Client Context.
	Identity func(ctx any) (id any, ok bool)
	// Resolve maps a wire identity to the currently materialized Client
	// Context.
	Resolve func(id any) (ctx any, ok bool)
}

// HostContextIdentity is the Host Context identity selected from the
// registered adapter set.
type HostContextIdentity struct {
	// Kind is the Context kind whose adapter recognized the Context.
	Kind string
	// Identity is the wire identity returned by that adapter.
	Identity any
}

// ChangeKind discriminates which registry store changed.
type ChangeKind string

// Change kinds.
const (
	ChangeLocal        ChangeKind = "local"
	ChangeRemote       ChangeKind = "remote"
	ChangeLookup       ChangeKind = "lookup"
	ChangeHostContext  ChangeKind = "host-context"
	ChangeClientContex ChangeKind = "client-context"
)

// RegistryChange is the notification emitted after the runtime registry
// changes.
type RegistryChange struct {
	Kind ChangeKind
	Key  string
}

// RegistryListener is one synchronous contained observer.
type RegistryListener func(change RegistryChange)

// TypertKey composes the global key of one generated schema:
// `<package>#<name>`.
func TypertKey(packageName, name string) string {
	return packageName + "#" + name
}

// TypertPackageKey composes the identity of one package-face model:
// `<package>#<face>`.
func TypertPackageKey(packageName string, face Face) string {
	return packageName + "#" + string(face)
}

// TypertEndpoint composes the endpoint key used by the local and Remote
// invocation registries: `<namespace>/<method>`.
func TypertEndpoint(descriptor InvocationDescriptor) string {
	return descriptor.Namespace + "/" + descriptor.Method
}

var wireNamePattern = regexp.MustCompile(`^[A-Za-z0-9_$.\-]+$`)

// validateWireName refuses values outside the RPC endpoint segment alphabet,
// plus the bare relative segments.
func validateWireName(subject, value string) error {
	if value == "." || value == ".." || !wireNamePattern.MatchString(value) {
		return fmt.Errorf("typert: invalid %s %q — must contain only RPC endpoint segment characters", subject, value)
	}
	return nil
}

// validateSegment refuses empty values and any "#".
func validateSegment(subject, value string) error {
	if len(value) == 0 || strings.Contains(value, "#") {
		return fmt.Errorf("typert: invalid %s %q — must be nonempty and must not contain %q", subject, value, "#")
	}
	return nil
}

// validateNonempty refuses empty values.
func validateNonempty(subject, value string) error {
	if len(value) == 0 {
		return fmt.Errorf("typert: invalid %s — must be nonempty", subject)
	}
	return nil
}

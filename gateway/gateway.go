// Package gateway dispatches live Typert Remote calls over Cordis Services
// and the registered lookup/Context providers of the Typert registry. The
// official carrier surfaces — Connection /api interception, the WebSocket
// Remote mux, and the Client face — adapt on top of this dispatcher.
//
// Honest degradation from @deepseek-ai/dsh-api-gateway: the SRC fallback is
// not portable. Its official form derives invocation descriptors from
// JavaScript method signatures at runtime (Function.prototype.toString
// parameter extraction plus @Remote markers), which has no Go equivalent —
// Go resolves strict registered definitions only, and unknown endpoints
// fail invocation-unavailable exactly as the official empty-candidate path.
// The typertRemote binding marker check is likewise unnecessary: the strict
// descriptor registration is itself the binding. Stream Remotes, the
// forwarded-event source, and the WebSocket mux land with the stream
// carrier round.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"dshgo/cordis"
	"dshgo/typert"
)

// GatewayErrorCode is one stable infrastructure or boundary failure
// category, distinct from business errors.
type GatewayErrorCode string

// The official error taxonomy.
const (
	CodeAmbiguousEndpoint     GatewayErrorCode = "gateway/ambiguous-endpoint"
	CodeArgumentsInvalid      GatewayErrorCode = "gateway/arguments-invalid"
	CodeBindingInvalid        GatewayErrorCode = "gateway/binding-invalid"
	CodeContextFailed         GatewayErrorCode = "gateway/context-failed"
	CodeContextNotFound       GatewayErrorCode = "gateway/context-not-found"
	CodeContextUnavailable    GatewayErrorCode = "gateway/context-unavailable"
	CodeDefinitionUnavailable GatewayErrorCode = "gateway/definition-unavailable"
	CodeInputInvalid          GatewayErrorCode = "gateway/input-invalid"
	CodeInvocationUnavailable GatewayErrorCode = "gateway/invocation-unavailable"
	CodeLookupFailed          GatewayErrorCode = "gateway/lookup-failed"
	CodeLookupNotFound        GatewayErrorCode = "gateway/lookup-not-found"
	CodeLookupUnavailable     GatewayErrorCode = "gateway/lookup-unavailable"
	CodeMethodUnavailable     GatewayErrorCode = "gateway/method-unavailable"
	CodeProviderMismatch      GatewayErrorCode = "gateway/provider-mismatch"
	CodeResultInvalid         GatewayErrorCode = "gateway/result-invalid"
	CodeServiceUnavailable    GatewayErrorCode = "gateway/service-unavailable"
	CodeSignatureInvalid      GatewayErrorCode = "gateway/signature-invalid"
)

// GatewayError is one dispatch failure produced outside the invoked
// business method. Its message never embeds boundary values.
type GatewayError struct {
	Code     GatewayErrorCode
	Endpoint string
	Field    string
	cause    error
	message  string
}

// Error renders the canonical gateway diagnostic.
func (e *GatewayError) Error() string {
	return fmt.Sprintf("typert gateway: %s: %s", e.Endpoint, e.message)
}

// Unwrap exposes the contained cause.
func (e *GatewayError) Unwrap() error { return e.cause }

func gatewayErrorf(code GatewayErrorCode, endpoint, field, format string, args ...any) *GatewayError {
	return &GatewayError{Code: code, Endpoint: endpoint, Field: field, message: fmt.Sprintf(format, args...)}
}

func wrapGatewayError(code GatewayErrorCode, endpoint, field string, cause error, format string, args ...any) *GatewayError {
	return &GatewayError{Code: code, Endpoint: endpoint, Field: field, cause: cause, message: fmt.Sprintf(format, args...)}
}

// invocationCancelled reports a business invocation that lost its carrier
// cancellation race; carriers map it to the RPC cancelled code.
type invocationCancelled struct {
	endpoint string
	cause    error
}

// Error names the raced invocation.
func (e *invocationCancelled) Error() string {
	return fmt.Sprintf("Remote invocation %q was aborted", e.endpoint)
}

// Unwrap exposes the business failure observed after cancellation.
func (e *invocationCancelled) Unwrap() error { return e.cause }

// InvokeRequest is one Remote method request after a carrier has decoded
// its envelope.
type InvokeRequest struct {
	// Namespace selected by the generated descriptor.
	Namespace string
	// Method is the exported Service method name.
	Method string
	// Args holds named wire values; keys must exactly match the descriptor.
	Args map[string]any
	// Signal is the carrier or direct-caller cancellation, injected only
	// into cancellation-aware methods.
	Signal context.Context
}

// Gateway is the carrier-independent Host dispatcher.
type Gateway struct {
	ctx      *cordis.Context
	registry *typert.Registry
	remote   remoteEventsState
}

// New binds the dispatcher to one Host context and its Typert registry.
func New(ctx *cordis.Context, registry *typert.Registry) *Gateway {
	return &Gateway{
		ctx:      ctx,
		registry: registry,
		remote: remoteEventsState{
			clients: map[string]*RemoteEventClient{},
			pending: map[string]*pendingRemoteEvent{},
		},
	}
}

// Invoke resolves the current descriptor and Cordis Service for the call,
// validates exact named arguments, resolves lookup and Context identities,
// invokes the business method, and hands back its result without output
// decoding.
func (g *Gateway) Invoke(signal context.Context, request InvokeRequest) (any, error) {
	prepared, err := g.prepareInvocation(signal, request)
	if err != nil {
		return nil, err
	}
	if prepared.descriptor.Mode == "stream" {
		return nil, gatewayErrorf(CodeSignatureInvalid, prepared.endpoint, "",
			"stream Remote methods must be opened through the stream carrier")
	}
	out := prepared.method.Call(prepared.args)
	// Go business convention: (result) or (result, error).
	var result any
	if len(out) > 0 {
		result = out[0].Interface()
	}
	if len(out) > 1 {
		if businessErr, ok := out[len(out)-1].Interface().(error); ok && businessErr != nil {
			if signal != nil && signal.Err() != nil {
				return nil, &invocationCancelled{endpoint: prepared.endpoint, cause: businessErr}
			}
			return nil, businessErr
		}
	}
	return result, nil
}

// WireFailure converts one dispatch or business failure to the carrier-safe
// Remote failure fields: cancellation races map to cancelled, lookup-policy
// and business Remote failures keep their envelope, everything else is
// internal.
func WireFailure(err error) typert.Failure {
	var cancelled *invocationCancelled
	if errors.As(err, &cancelled) {
		return typert.Failure{Code: "cancelled", Message: cancelled.Error(), Details: map[string]any{}}
	}
	var lookup *typert.LookupFailure
	if errors.As(err, &lookup) {
		return lookup.Failure
	}
	var remote *typert.RemoteFailure
	if errors.As(err, &remote) {
		return remote.Failure
	}
	var gatewayErr *GatewayError
	if errors.As(err, &gatewayErr) {
		details := map[string]any{"endpoint": gatewayErr.Endpoint}
		if gatewayErr.Field != "" {
			details["field"] = gatewayErr.Field
		}
		return typert.Failure{Code: string(gatewayErr.Code), Message: gatewayErr.Error(), Details: details}
	}
	return typert.Failure{Code: "internal", Message: err.Error(), Details: map[string]any{}}
}

type preparedInvocation struct {
	endpoint   string
	descriptor typert.InvocationDescriptor
	receiver   any
	args       []reflect.Value
	method     reflect.Value
}

func (g *Gateway) prepareInvocation(signal context.Context, request InvokeRequest) (*preparedInvocation, error) {
	endpoint := request.Namespace + "/" + request.Method
	if request.Namespace == "" || request.Method == "" {
		return nil, fmt.Errorf("invalid Remote endpoint %q", endpoint)
	}
	descriptor, err := g.resolveDescriptor(request, endpoint)
	if err != nil {
		return nil, err
	}
	if err := assertExactArguments(request.Args, descriptor, endpoint); err != nil {
		return nil, err
	}
	receiverContext, err := g.resolveReceiverContext(signal, descriptor, request.Args, endpoint)
	if err != nil {
		return nil, err
	}
	receiver := receiverContext.Get(descriptor.Service)
	if receiver == nil {
		return nil, gatewayErrorf(CodeServiceUnavailable, endpoint, "",
			"active Service %q is unavailable", descriptor.Service)
	}
	implementation := descriptor.Method
	if descriptor.Implementation != "" {
		implementation = descriptor.Implementation
	}
	method := reflect.ValueOf(receiver).MethodByName(implementation)
	if !method.IsValid() {
		return nil, gatewayErrorf(CodeMethodUnavailable, endpoint, "",
			"active Service %q has no callable method %q", descriptor.Service, implementation)
	}
	// Cancellation injects as the FIRST argument: official JS appends the
	// signal last, but Go business methods take context.Context first.
	args := make([]reflect.Value, 0, len(descriptor.Parameters)+1)
	if descriptor.CancellationParameter != "" {
		if signal == nil {
			signal = context.Background()
		}
		args = append(args, reflect.ValueOf(signal))
	}
	for _, parameter := range descriptor.Parameters {
		value, err := g.resolveParameter(parameter, request.Args, endpoint)
		if err != nil {
			return nil, err
		}
		// An absent optional parameter carries the method parameter's
		// typed zero — reflect cannot take an untyped nil.
		if value == nil {
			offset := len(args)
			if offset < method.Type().NumIn() {
				args = append(args, reflect.Zero(method.Type().In(offset)))
			} else {
				args = append(args, reflect.Value{})
			}
			continue
		}
		args = append(args, reflect.ValueOf(value))
	}
	return &preparedInvocation{
		endpoint:   endpoint,
		descriptor: descriptor,
		receiver:   receiver,
		args:       args,
		method:     method,
	}, nil
}

// resolveDescriptor resolves one strict registered definition. The official
// SRC fallback (JavaScript signature extraction) has no Go equivalent, so a
// never-registered endpoint fails exactly as the official empty-candidate
// path, and a withdrawn definition still refuses to weaken validation.
func (g *Gateway) resolveDescriptor(request InvokeRequest, endpoint string) (typert.InvocationDescriptor, error) {
	if descriptor, ok := g.registry.LocalGet(endpoint); ok {
		return descriptor, nil
	}
	if g.registry.LocalHasSeen(endpoint) {
		return typert.InvocationDescriptor{}, gatewayErrorf(CodeDefinitionUnavailable, endpoint, "",
			"its strict definition was withdrawn and SRC fallback is forbidden")
	}
	return typert.InvocationDescriptor{}, gatewayErrorf(CodeInvocationUnavailable, endpoint, "",
		"no active Remote method exports this endpoint")
}

func (g *Gateway) resolveReceiverContext(signal context.Context, descriptor typert.InvocationDescriptor, args map[string]any, endpoint string) (*cordis.Context, error) {
	if descriptor.Invocation.Kind == typert.ReceiverDirect {
		return g.ctx, nil
	}
	invocation := descriptor.Invocation
	adapter, ok := g.registry.ContextGetHost(invocation.Context)
	if !ok {
		return nil, gatewayErrorf(CodeContextUnavailable, endpoint, "",
			"Context provider %q is unavailable", invocation.Context)
	}
	if adapter.Wire != invocation.Wire ||
		(invocation.Codec.Mode == typert.CodecStrict && adapter.WireTypeSymbol != invocation.Codec.TypeSymbol) {
		return nil, gatewayErrorf(CodeProviderMismatch, endpoint, invocation.Wire,
			"Context provider %q does not match its strict definition", invocation.Context)
	}
	identity, err := decode(invocation.Codec, args[invocation.Wire], endpoint, invocation.Wire)
	if err != nil {
		return nil, err
	}
	resolved, found, resolveErr := adapter.Resolve(identity)
	if resolveErr != nil {
		var lookupFailure *typert.LookupFailure
		if errors.As(resolveErr, &lookupFailure) {
			return nil, resolveErr
		}
		return nil, wrapGatewayError(CodeContextFailed, endpoint, invocation.Wire, resolveErr,
			"Context provider %q failed", invocation.Context)
	}
	if !found {
		return nil, gatewayErrorf(CodeContextNotFound, endpoint, invocation.Wire,
			"Context provider %q did not resolve the requested identity", invocation.Context)
	}
	context, ok := resolved.(*cordis.Context)
	if !ok {
		return nil, gatewayErrorf(CodeContextFailed, endpoint, invocation.Wire,
			"Context provider %q resolved a non-Context value", invocation.Context)
	}
	return context, nil
}

func (g *Gateway) resolveParameter(parameter typert.InvocationParameterDescriptor, args map[string]any, endpoint string) (any, error) {
	// An absent wire field reached assertExactArguments' allowance, so this
	// parameter takes a nil value; lookup ids are never omissible, so
	// absence only ever belongs to a json parameter.
	value, present := args[parameter.Wire]
	if !present {
		return nil, nil
	}
	decoded, err := decode(parameter.Codec, value, endpoint, parameter.Wire)
	if err != nil {
		return nil, err
	}
	if parameter.Source == typert.SourceJSON {
		return decoded, nil
	}
	key := parameter.Lookup
	if key == "" {
		return nil, gatewayErrorf(CodeLookupUnavailable, endpoint, parameter.Wire,
			"lookup parameter %q has no provider key", parameter.Name)
	}
	provider, ok := g.registry.LookupGet(key)
	if !ok {
		return nil, gatewayErrorf(CodeLookupUnavailable, endpoint, parameter.Wire,
			"lookup provider %q is unavailable", key)
	}
	if provider.Wire != parameter.Wire ||
		(parameter.Codec.Mode == typert.CodecStrict && provider.WireTypeSymbol != parameter.Codec.TypeSymbol) {
		return nil, gatewayErrorf(CodeProviderMismatch, endpoint, parameter.Wire,
			"lookup provider %q does not match its strict definition", key)
	}
	resolved, resolveErr := provider.Resolve(decoded)
	if resolveErr != nil {
		var lookupFailure *typert.LookupFailure
		if errors.As(resolveErr, &lookupFailure) {
			return nil, resolveErr
		}
		return nil, wrapGatewayError(CodeLookupFailed, endpoint, parameter.Wire, resolveErr,
			"lookup provider %q failed", key)
	}
	if resolved == nil {
		return nil, gatewayErrorf(CodeLookupNotFound, endpoint, parameter.Wire,
			"lookup provider %q did not resolve the requested identity", key)
	}
	return resolved, nil
}

// assertExactArguments enforces the descriptor's exact named wire fields.
// A JSON field may be absent when the strict descriptor declares absence;
// lookup ids are never omissible.
func assertExactArguments(args map[string]any, descriptor typert.InvocationDescriptor, endpoint string) error {
	expected := map[string]bool{}
	for _, parameter := range descriptor.Parameters {
		expected[parameter.Wire] = true
	}
	if descriptor.Invocation.Kind == typert.ReceiverContext {
		expected[descriptor.Invocation.Wire] = true
	}
	acceptsMissing := map[string]bool{}
	for _, parameter := range descriptor.Parameters {
		if parameter.Source == typert.SourceJSON &&
			(parameter.AcceptsUndefined || parameter.Codec.Mode == typert.CodecSrcJSON) {
			acceptsMissing[parameter.Wire] = true
		}
	}
	var extra []string
	for key := range args {
		if !expected[key] {
			extra = append(extra, fmt.Sprintf("%q", key))
		}
	}
	var missing []string
	for key := range expected {
		if _, present := args[key]; !present && !acceptsMissing[key] {
			missing = append(missing, fmt.Sprintf("%q", key))
		}
	}
	if len(extra) == 0 && len(missing) == 0 {
		return nil
	}
	sortStrings(extra)
	sortStrings(missing)
	clauses := ""
	if len(missing) > 0 {
		clauses += "missing " + joinCommas(missing)
	}
	if len(extra) > 0 {
		if clauses != "" {
			clauses += "; "
		}
		clauses += "unexpected " + joinCommas(extra)
	}
	return gatewayErrorf(CodeArgumentsInvalid, endpoint, "",
		"args fields do not match the descriptor: %s", clauses)
}

// decode passes one wire value through its boundary: the JSON marshal is
// the JSON-safety assertion (cycles, non-finite numbers, and unsupported
// types fail), and strict codecs additionally run their validator over the
// encoded form.
func decode(codec typert.Codec, value any, endpoint, field string) (any, error) {
	if value == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, wrapGatewayError(CodeInputInvalid, endpoint, field, err,
			"wire field %q failed boundary validation", field)
	}
	if codec.Mode == typert.CodecStrict && codec.Validate != nil {
		if err := codec.Validate(encoded); err != nil {
			return nil, wrapGatewayError(CodeInputInvalid, endpoint, field, err,
				"wire field %q failed boundary validation", field)
		}
	}
	return value, nil
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func joinCommas(values []string) string {
	out := ""
	for i, value := range values {
		if i > 0 {
			out += ", "
		}
		out += value
	}
	return out
}

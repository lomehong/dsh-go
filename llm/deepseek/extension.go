// DeepSeek LLM API extension registry: plugins own independent top-level
// request fields while the official adapter performs one preparation and
// acceptance transaction.
package deepseek

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// RequestFacts are the exact serialized request facts visible to extension
// providers. Body is the base DeepSeek request body before extension fields
// are merged; SessionID and Purpose are present only when the model request
// carried them.
type RequestFacts struct {
	Body      map[string]any
	SessionID string
	Purpose   string
	// Signal cancels request preparation; providers must stop promptly
	// after abort. Go adaptation of the official AbortSignal.
	Signal context.Context
}

// FieldValue is one prepared field: the detached value merged under the
// provider's registered field, plus the optional post-2xx commit.
type FieldValue struct {
	Value any
	// Accept commits state that depends on confirmed provider acceptance.
	// Nil means nothing to commit.
	Accept func() error
}

// FieldProvider prepares one field for an exact serialized request. Ok is
// false when this request has no value for the field.
type FieldProvider interface {
	Prepare(request RequestFacts) (value FieldValue, ok bool, err error)
}

// FieldProviderFunc adapts a function to FieldProvider.
type FieldProviderFunc func(request RequestFacts) (value FieldValue, ok bool, err error)

// Prepare implements FieldProvider.
func (fn FieldProviderFunc) Prepare(request RequestFacts) (FieldValue, bool, error) {
	return fn(request)
}

// PreparedExtensions carries every field prepared for one request plus their
// joint acceptance transaction.
type PreparedExtensions struct {
	// Fields are the detached top-level fields to merge into the base
	// request body.
	Fields map[string]any

	accepts  []func() error
	once     sync.Once
	accepted error
} // Accept commits every captured provider after HTTP 2xx. Repeated calls
// join the same settlement. Every callback runs before failures report: one
// failure returns itself, several join one aggregate error.
func (p *PreparedExtensions) Accept() error {
	if p == nil {
		return nil
	}
	p.once.Do(func() {
		var failures []error
		for _, accept := range p.accepts {
			if err := accept(); err != nil {
				failures = append(failures, err)
			}
		}
		switch len(failures) {
		case 0:
		case 1:
			p.accepted = failures[0]
		default:
			p.accepted = fmt.Errorf("DeepSeek LLM API extension acceptance failed: %w", errors.Join(failures...))
		}
	})
	return p.accepted
}

// ExtensionRegistry is the registry of independently owned top-level fields
// for official DeepSeek requests. It is safe for concurrent use.
type ExtensionRegistry struct {
	mu        sync.Mutex
	providers map[string]FieldProvider
}

// NewExtensionRegistry builds an empty registry.
func NewExtensionRegistry() *ExtensionRegistry {
	return &ExtensionRegistry{providers: map[string]FieldProvider{}}
}

// Register the sole provider of one top-level request field. The field must
// be a non-blank trimmed string, and each field has exactly one provider:
// a duplicate registration fails loud. The returned disposer releases the
// field.
func (r *ExtensionRegistry) Register(field string, provider FieldProvider) (func(), error) {
	if field == "" || strings.TrimSpace(field) != field {
		return nil, fmt.Errorf("deepseek-llm-api-extensions: field must be a non-blank trimmed string")
	}
	if provider == nil {
		return nil, fmt.Errorf("deepseek-llm-api-extensions: field %q registered without a provider", field)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, taken := r.providers[field]; taken {
		return nil, fmt.Errorf("deepseek-llm-api-extensions: field %q is already registered", field)
	}
	r.providers[field] = provider
	return func() {
		r.mu.Lock()
		delete(r.providers, field)
		r.mu.Unlock()
	}, nil
}

// Prepare every currently registered field from one immutable base request.
// Preparation failures reject before HTTP dispatch. An aborted signal
// cancels the whole preparation. Fields arrive detached; providers retain no
// mutable alias to the outgoing request.
func (r *ExtensionRegistry) Prepare(ctx context.Context, request RequestFacts) (*PreparedExtensions, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	fields := make([]string, 0, len(r.providers))
	providers := make(map[string]FieldProvider, len(r.providers))
	for name, provider := range r.providers {
		fields = append(fields, name)
		providers[name] = provider
	}
	r.mu.Unlock()
	sort.Strings(fields)

	prepared := &PreparedExtensions{Fields: map[string]any{}}
	for _, field := range fields {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		value, ok, err := providers[field].Prepare(request)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		prepared.Fields[field] = value.Value
		if value.Accept != nil {
			prepared.accepts = append(prepared.accepts, value.Accept)
		}
	}
	return prepared, nil
}

// cloneJSONValue deep-copies one lossless JSON value so providers can keep
// no mutable alias into the outgoing request.
func cloneJSONValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		cloned := make(map[string]any, len(typed))
		for key, item := range typed {
			cloned[key] = cloneJSONValue(item)
		}
		return cloned
	case []any:
		cloned := make([]any, len(typed))
		for index, item := range typed {
			cloned[index] = cloneJSONValue(item)
		}
		return cloned
	default:
		return value
	}
}

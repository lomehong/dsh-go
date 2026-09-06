// Package gojaruntime implements the coderuntime.CodeRuntime seam over the
// goja pure-Go ES2015+ JavaScript engine. Runs execute in-process (no CGO,
// no subprocess): each Run creates a fresh goja.Runtime, installs host
// bindings as global functions, and evaluates the program. A hard per-run
// timeout (goja.Interrupt) terminates runaway programs.
package gojaruntime

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/dop251/goja"

	"dshgo/coderuntime"
)

// Config configures the goja runtime.
type Config struct {
	TimeoutMs int64
}

// Run is the goja-backed CodeRuntime.
type Run struct {
	config Config
	mu     sync.Mutex
	closed bool
}

func New(config Config) *Run {
	if config.TimeoutMs <= 0 {
		config.TimeoutMs = 30000
	}
	return &Run{config: config}
}

func (r *Run) Language() string  { return "javascript" }
func (r *Run) Isolation() string { return "in-process-goja" }

func (r *Run) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	return nil
}

func (r *Run) Run(request coderuntime.CodeRunRequest) (coderuntime.CodeRunResult, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return coderuntime.CodeRunResult{}, fmt.Errorf("gojaruntime: runtime is closed")
	}
	r.mu.Unlock()

	if request.Signal != nil && request.Signal.Err() != nil {
		return coderuntime.CodeRunResult{
			Error: &coderuntime.CodeRunFailure{Kind: coderuntime.FailureAbort, Message: "pre-aborted"},
		}, nil
	}

	timeout := time.Duration(r.config.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	vm := goja.New()
	timer := time.AfterFunc(timeout, func() { vm.Interrupt("execution timed out") })
	defer timer.Stop()

	installBindings(vm, request.Bindings)

	source := fmt.Sprintf("(async function(){\n%s\n})()", request.Program)
	completion, err := vm.RunString(source)
	if err != nil {
		if isInterrupt(err) {
			return coderuntime.CodeRunResult{
				Error: &coderuntime.CodeRunFailure{Kind: coderuntime.FailureTimeout, Message: "execution timed out"},
			}, nil
		}
		return coderuntime.CodeRunResult{
			Error: &coderuntime.CodeRunFailure{Kind: coderuntime.FailureException, Message: err.Error()},
		}, nil
	}
	return extractResult(vm, completion), nil
}

// extractResult resolves the completion value: a *goja.Promise is settled
// synchronously for non-I/O programs (goja resolves promises on the event
// loop), so we read its state and extract the settled result. Rejected
// promises become FailureException (async-body throws must not be
// silently swallowed).
func extractResult(vm *goja.Runtime, completion goja.Value) coderuntime.CodeRunResult {
	if promise, ok := completion.Export().(*goja.Promise); ok {
		switch promise.State() {
		case goja.PromiseStateFulfilled:
			return resultFromGoja(promise.Result().Export())
		case goja.PromiseStateRejected:
			reason := promise.Result()
			msg := "async program rejected"
			if reason != nil {
				msg = reason.String()
			}
			return coderuntime.CodeRunResult{
				Error: &coderuntime.CodeRunFailure{Kind: coderuntime.FailureException, Message: msg},
			}
		default:
			// Pending: a program that awaits an unresolved binding call.
			return coderuntime.CodeRunResult{
				Error: &coderuntime.CodeRunFailure{Kind: coderuntime.FailureTimeout, Message: "program did not settle within the run"},
			}
		}
	}
	return resultFromGoja(completion.Export())
}

func installBindings(vm *goja.Runtime, bindings []coderuntime.CodeBindingNamespace) {
	for _, ns := range bindings {
		obj := vm.NewObject()
		for name, fn := range ns.Functions {
			fnCopy := fn
			_ = obj.Set(name, func(call goja.FunctionCall) goja.Value {
				args := exportJSON(call.Argument(0))
				result, bindErr := fnCopy(args)
				if bindErr != nil {
					panic(vm.NewGoError(bindErr))
				}
				return vm.ToValue(result)
			})
		}
		vm.Set(ns.Global, obj)
	}
}

func resultFromGoja(exported any) coderuntime.CodeRunResult {
	switch v := exported.(type) {
	case nil:
	case string:
		return coderuntime.CodeRunResult{Value: v}
	case bool:
		return coderuntime.CodeRunResult{Value: v}
	case float64:
		return coderuntime.CodeRunResult{Value: v}
	case int:
		return coderuntime.CodeRunResult{Value: float64(v)}
	case []any:
		return coderuntime.CodeRunResult{Value: v}
	case map[string]any:
		return coderuntime.CodeRunResult{Value: v}
	default:
		if b, jsonErr := json.Marshal(exported); jsonErr == nil {
			var generic any
			if json.Unmarshal(b, &generic) == nil {
				return coderuntime.CodeRunResult{Value: generic}
			}
		}
	}
	return coderuntime.CodeRunResult{}
}

func exportJSON(value goja.Value) coderuntime.CodeJSONValue {
	if value == nil {
		return nil
	}
	exported := value.Export()
	if b, err := json.Marshal(exported); err == nil {
		var out any
		if json.Unmarshal(b, &out) == nil {
			return out
		}
	}
	return exported
}

func isInterrupt(err error) bool {
	_, ok := err.(*goja.InterruptedError)
	return ok
}

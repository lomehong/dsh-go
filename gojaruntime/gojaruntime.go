// Package gojaruntime implements the coderuntime.CodeRuntime seam over the
// goja pure-Go ES2015+ JavaScript engine. Runs execute in-process (no CGO,
// no subprocess): each Run creates a fresh goja.Runtime, installs host
// bindings as global functions, and evaluates the program as an async
// function body (top-level await and return available). A hard per-run
// timeout (goja.Interrupt) terminates runaway programs.
//
// This is the Go-native answer to the official code-runtime-worker-thread
// (which requires Node.js worker_threads): same CodeRuntime contract, pure
// Go execution substrate.
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

// Run is the goja-backed CodeRuntime: fresh VM per run, host bindings as
// globals, hard timeout via goja.Interrupt.
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

func (r *Run) Language() string { return "typescript" }
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
	if timeout <= 0 { timeout = 30 * time.Second }
	vm := goja.New()
	timer := time.AfterFunc(timeout, func() { vm.Interrupt("execution timed out") })
	defer timer.Stop()
	installBindings(vm, request.Bindings)

	source := fmt.Sprintf("(async function(){\n%s\n})()", request.Program)
	completion, err := vm.RunString(source)
	if err != nil {
		if isInterrupt(err) {
			return coderuntime.CodeRunResult{Error: &coderuntime.CodeRunFailure{Kind: coderuntime.FailureTimeout, Message: "execution timed out"}}, nil
		}
		return coderuntime.CodeRunResult{Error: &coderuntime.CodeRunFailure{Kind: coderuntime.FailureException, Message: err.Error()}}, nil
	}
	return resultFromGoja(completion.Export()), nil
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
	case int64:
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

func exportJSON(value goja.Value) coderuntime.CodeJSONValue {
	if value == nil { return nil }
	exported := value.Export()
	if b, err := json.Marshal(exported); err == nil {
		var out any
		if json.Unmarshal(b, &out) == nil { return out }
	}
	return exported
}

func isInterrupt(err error) bool {
	_, ok := err.(*goja.InterruptedError)
	return ok
}

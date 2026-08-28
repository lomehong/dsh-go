package loader

import (
	"fmt"
	"sync"
)

// RawExpression preserves one `!!js` scalar verbatim. The official loader
// evaluates these at entry activation; without a JavaScript evaluator
// installed any evaluation fails loudly instead of guessing.
type RawExpression string

// String renders the expression source.
func (e RawExpression) String() string { return string(e) }

// isJsTag reports whether a YAML scalar carries the JavaScript expression
// tag, in either the shorthand form yaml.v3 preserves (`!!js`) or the
// resolved form the official js-yaml schema produces
// (`tag:yaml.org,2002:js`).
func isJsTag(tag string) bool {
	return tag == "!!js" || tag == "tag:yaml.org,2002:js"
}

var (
	evaluatorMu sync.RWMutex
	jsEvaluator func(RawExpression) (any, error)
)

// SetJSEvaluator installs the interpreter for `!!js` expression nodes. The
// official runtime evaluates them against the loader context; a Go host wires
// a pure-Go JS engine here (or a literal-only evaluator in constrained
// deployments). nil removes the evaluator.
func SetJSEvaluator(fn func(RawExpression) (any, error)) {
	evaluatorMu.Lock()
	defer evaluatorMu.Unlock()
	jsEvaluator = fn
}

// Evaluate resolves one expression node. Without an evaluator it fails
// loudly: silently treating an expression as a literal value would write
// unevaluated config into a running entry.
func Evaluate(expr RawExpression) (any, error) {
	evaluatorMu.RLock()
	fn := jsEvaluator
	evaluatorMu.RUnlock()
	if fn == nil {
		return nil, fmt.Errorf("loader: !!js expression %q cannot be evaluated: no JavaScript evaluator registered", string(expr))
	}
	return fn(expr)
}

// IsDisabled resolves an entry's disabled flag: literal booleans pass
// through, expressions go through the registered evaluator.
func IsDisabled(entry Entry) (bool, error) {
	switch value := entry.Disabled.(type) {
	case nil:
		return false, nil
	case bool:
		return value, nil
	case RawExpression:
		resolved, err := Evaluate(value)
		if err != nil {
			return false, err
		}
		flag, ok := resolved.(bool)
		if !ok {
			return false, fmt.Errorf("loader: entry %q disabled expression must resolve to a bool, got %T", entry.ID, resolved)
		}
		return flag, nil
	default:
		return false, fmt.Errorf("loader: entry %q disabled must be a bool or a !!js expression, got %T", entry.ID, value)
	}
}

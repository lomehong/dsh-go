// The `!!js` platform-predicate evaluator: the Go host's constrained
// interpreter for loader expressions. The official runtime evaluates
// JavaScript against the loader context; a Go host cannot run JS, so the
// evaluator recognizes exactly the platform predicates the shipped bundles
// use (`process.platform === 'win32'` and its negation) and refuses
// everything else loudly — an unevaluable expression must never be silently
// treated as a literal (that would write wrong config into a running entry).
package boot

import (
	"fmt"
	"runtime"
	"strings"

	"dshgo/cordis/loader"
)

// RegisterPlatformEvaluator installs the constrained platform-predicate
// evaluator as the loader's `!!js` interpreter. Call once at assembly; the
// evaluator is process-global (loader.SetJSEvaluator semantics).
func RegisterPlatformEvaluator() {
	loader.SetJSEvaluator(func(expr loader.RawExpression) (any, error) {
		raw := strings.TrimSpace(expr.String())
		switch raw {
		case `process.platform === 'win32'`:
			return runtime.GOOS == "windows", nil
		case `process.platform !== 'win32'`:
			return runtime.GOOS != "windows", nil
		default:
			return nil, fmt.Errorf("loader: !!js expression %q is not a supported platform predicate (the Go host evaluates only process.platform === / !== 'win32')", expr.String())
		}
	})
}

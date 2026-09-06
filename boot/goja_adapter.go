// goja adapter: wraps gojaruntime.Run as the tools.CodeRuntime face so
// PTC-mode run_code dispatches execute through the goja engine.
package boot

import (
	"dshgo/coderuntime"
	"dshgo/gojaruntime"
	"dshgo/tools"
)

// gojaCodeRuntime adapts gojaruntime.Run to the tools.CodeRuntime face.
type gojaCodeRuntime struct{ run *gojaruntime.Run }

func newGojaCodeRuntime() *gojaCodeRuntime {
	return &gojaCodeRuntime{run: gojaruntime.New(gojaruntime.Config{})}
}

func (g *gojaCodeRuntime) Run(request coderuntime.CodeRunRequest) (coderuntime.CodeRunResult, error) {
	return g.run.Run(request)
}

func (g *gojaCodeRuntime) Language() string  { return g.run.Language() }
func (g *gojaCodeRuntime) Isolation() string { return g.run.Isolation() }
func (g *gojaCodeRuntime) Close() error      { return g.run.Close() }

// compile-time assertion
var _ tools.CodeRuntime = (*gojaCodeRuntime)(nil)

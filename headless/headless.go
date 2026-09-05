// Package headless re-implements @deepseek-ai/dsh-headless (official tag
// dsh-v0.1.3-alpha.1): the one-shot direct Agent driver. The bundle rides
// over dsh-base without Host, HTTP, or browser plugins; the runner creates
// one Agent through the core registry, drives the task to quiescence,
// flushes its Session, prints the final assistant text to stdout, and
// requests process exit.
package headless

import (
	"os"
	"strings"
	"sync/atomic"

	"dshgo/llm"
)

// StartupService is the cordis service name the startup row provides and
// the runner row's config reads (official HEADLESS_STARTUP_SERVICE).
const StartupService = "headlessStartup"

// ExitService is the host value the runner reads to request the process
// exit code after its run (official ctx.appExit).
const ExitService = "appExit"

// StartupValues is what the runner row reads from the startup service.
type StartupValues struct {
	// Task is the task text this invocation asked for.
	Task string `json:"task"`
}

// exitFunc is the process-facing exit request signature.
type exitFunc func(code int)

// exitCode carries the requested process exit code to the launcher.
var exitCode atomic.Int64

// ExitCode returns the exit code the headless run requested (0 before any
// request; the launcher reads it after the tree disposes).
func ExitCode() int { return int(exitCode.Load()) }

// RequestExit records the requested exit code.
func RequestExit(code int) { exitCode.Store(int64(code)) }

// ParseTask extracts the one-shot task from the launcher's inner arguments:
// the first non-flag positional and everything after it, joined by spaces
// (official commander `[task...]` positional semantics for this app's one
// flag-free command). The second result reports whether --help was asked.
func ParseTask(args []string) (task string, help bool) {
	rest := []string{}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch arg {
		case "-h", "--help":
			return "", true
		case "--":
			rest = append(rest, args[index+1:]...)
			index = len(args)
		default:
			if strings.HasPrefix(arg, "-") {
				// Known launcher flags carry values; skip them and their
				// value so a value-looking task word is not consumed. The
				// shipped headless command has no options of its own, so
				// any other flag (e.g. -profile/--home handled by the
				// launcher) ends value-skipping only for these forms.
				continue
			}
			rest = append(rest, args[index:]...)
			index = len(args)
		}
	}
	return strings.Join(rest, " "), false
}

// assistantText aggregates the newest non-empty assistant text from one
// event window (official summarize): text-only blocks joined in order; the
// last message with visible text wins.
func assistantText(content []llm.ContentBlock) string {
	joined := ""
	for _, block := range content {
		if block.Type == llm.BlockText {
			joined += block.Text
		}
	}
	return joined
}

// Stdout/Stderr are the process-facing output streams; tests substitute
// captures (official internals).
var (
	Stdout = os.Stdout
	Stderr = os.Stderr
)

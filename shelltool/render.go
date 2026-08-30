package shelltool

import (
	"fmt"
	"strings"

	"dshgo/shell"
	"dshgo/subprocess"
)

// streamText appends the truncation notice (with the full-output spill
// path) to a stream's text.
func streamText(output subprocess.CollectedOutput) string {
	if !output.Truncated {
		return output.Text
	}
	spill := output.SpillPath
	if spill == "" {
		spill = "(unavailable)"
	}
	return fmt.Sprintf("%s\n[output truncated; full output: %s]", output.Text, spill)
}

// RenderResult shapes one finished run into the text the model sees:
// stdout, then a marked stderr section, then exit-status markers. Non-zero
// exits are reported, not errored — the model decides how to react; only
// infrastructure failures (spawn errors, aborts) surface as error results.
func RenderResult(result shell.ShellRunResult) string {
	out := streamText(result.Stdout)
	errText := streamText(result.Stderr)

	body := out
	if errText != "" {
		// Single newline between sections (stdout usually ends with one
		// already).
		if body != "" && !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		body += "[stderr]\n" + errText
	}
	if body == "" {
		body = "(no output)"
	}

	var markers []string
	// A command may trap SIGTERM and exit 0 after timeout; still report
	// interruption. Keep the exit marker last because parseExitStatus
	// anchors there.
	if result.TimedOut {
		markers = append(markers, fmt.Sprintf("[timed out after %dms]", result.TimeoutMs))
	}
	if result.Signal != "" {
		markers = append(markers, "[killed by signal: "+result.Signal+"]")
	} else if result.ExitCode != 0 {
		markers = append(markers, fmt.Sprintf("[exit code: %d]", result.ExitCode))
	}
	if len(markers) == 0 {
		return body
	}
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return body + strings.Join(markers, "\n")
}

// RenderProcessRead shapes one background-process read into the
// `job_output` delta the model sees: the incremental delta, plus the
// lossy-read notice (with full-stream spill paths) when in-memory
// truncation dropped unread bytes. Empty-delta rendering (`(no new
// output)`) is the generic job controller's job.
func RenderProcessRead(read shell.ShellProcessRead) string {
	var notices []string
	if read.Lossy {
		var paths []string
		if read.StdoutSpillPath != "" {
			paths = append(paths, read.StdoutSpillPath)
		}
		if read.StderrSpillPath != "" {
			paths = append(paths, read.StderrSpillPath)
		}
		joined := "(unavailable)"
		if len(paths) > 0 {
			joined = strings.Join(paths, ", ")
		}
		notices = append(notices, "[some output was dropped from memory; full output: "+joined+"]")
	}
	if len(notices) == 0 {
		return read.Delta
	}
	separator := ""
	if read.Delta != "" && !strings.HasSuffix(read.Delta, "\n") {
		separator = "\n"
	}
	return read.Delta + separator + strings.Join(notices, "\n")
}

// renderForeground renders the canonical foreground outcome map back into
// the model-facing text (the render path consumes the post-clone
// canonical shape).
func renderForeground(outcome map[string]any) string {
	result := shell.ShellRunResult{
		ExitCode:  numberOrMinusOne(outcome["exitCode"]),
		Signal:    stringOrEmpty(outcome["signal"]),
		TimedOut:  boolOrFalse(outcome["timedOut"]),
		Aborted:   boolOrFalse(outcome["aborted"]),
		TimeoutMs: numberOrZero(outcome["timeoutMs"]),
		Stdout:    streamFromMap(outcome["stdout"]),
		Stderr:    streamFromMap(outcome["stderr"]),
	}
	return RenderResult(result)
}

func streamFromMap(raw any) subprocess.CollectedOutput {
	m, ok := raw.(map[string]any)
	if !ok {
		return subprocess.CollectedOutput{}
	}
	text, _ := m["text"].(string)
	truncated, _ := m["truncated"].(bool)
	spillPath, _ := m["spillPath"].(string)
	return subprocess.CollectedOutput{Text: text, Truncated: truncated, SpillPath: spillPath}
}

func numberOrMinusOne(raw any) int {
	if raw == nil {
		return -1
	}
	return numberOrZero(raw)
}

func numberOrZero(raw any) int {
	switch v := raw.(type) {
	case int:
		return v
	case float64:
		return int(v)
	}
	return 0
}

func stringOrEmpty(raw any) string {
	if s, ok := raw.(string); ok {
		return s
	}
	return ""
}

func boolOrFalse(raw any) bool {
	if b, ok := raw.(bool); ok {
		return b
	}
	return false
}

// Gate script: the single pre-commit check every submission must pass.
// Runs the full verification battery in order and reports a single
// PASS/FAIL verdict. Any failure aborts with a non-zero exit code.
//
// Usage: go run ./scripts/gate
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

type step struct {
	name string
	args []string
}

func main() {
	steps := []step{
		{"gofmt", []string{"gofmt", "-l", "."}},
		{"build", []string{"go", "build", "./..."}},
		{"vet", []string{"go", "vet", "./..."}},
		{"test", []string{"go", "test", "./...", "-count=1"}},
		{"genstatus", []string{"go", "run", "./scripts/genstatus"}},
	}

	start := time.Now()
	failed := false
	for _, s := range steps {
		fmt.Printf("==> %s\n", s.name)
		cmd := exec.Command(s.args[0], s.args[1:]...)
		output, err := cmd.CombinedOutput()
		text := strings.TrimSpace(string(output))

		switch s.name {
		case "gofmt":
			// gofmt -l prints offending files; empty output = clean.
			if text != "" {
				failed = true
				fmt.Printf("    FAIL: unformatted files:\n%s\n", indent(text))
			} else {
				fmt.Println("    ok (no unformatted files)")
			}
		default:
			if err != nil {
				failed = true
				fmt.Printf("    FAIL: %v\n%s\n", err, indent(text))
			} else {
				fmt.Printf("    ok (%s)\n", lastLine(text))
			}
		}
		if failed {
			break
		}
	}

	elapsed := time.Since(start).Round(time.Second)
	if failed {
		fmt.Printf("\nGATE: FAIL (%s)\n", elapsed)
		os.Exit(1)
	}
	fmt.Printf("\nGATE: PASS (%s)\n", elapsed)
}

func indent(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = "    " + line
	}
	return strings.Join(lines, "\n")
}

func lastLine(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1]
}

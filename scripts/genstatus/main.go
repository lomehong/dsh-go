// genstatus regenerates the derived-numbers block in STATUS.md between the
// BEGIN/END GENERATED markers: toolchain facts, package/test counts, and the
// wired catalog-entry count. Humans edit STATUS.md prose; counters come from
// here so the ledger cannot drift from the tree.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "genstatus: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	root, err := os.Getwd()
	if err != nil {
		return err
	}

	// Packages and test-file presence straight from the go tool (no build).
	out, err := execGo(root, "list", "-f", "{{.ImportPath}}|{{len .TestGoFiles}}|{{len .XTestGoFiles}}", "./...")
	if err != nil {
		return err
	}
	packages := 0
	withTests := 0
	dirs := []string{}
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(strings.TrimSpace(line), "|")
		if len(parts) != 3 {
			continue
		}
		packages++
		if parts[1] != "0" || parts[2] != "0" {
			withTests++
			if dir, dirErr := packageDir(root, parts[0]); dirErr == nil {
				dirs = append(dirs, dir)
			}
		}
	}

	// Behavior-test count: `func Test` declarations across test files.
	testRe := regexp.MustCompile(`(?m)^func (Test|Fuzz)\w+\(`)
	tests := 0
	for _, dir := range dirs {
		entries, readErr := os.ReadDir(dir)
		if readErr != nil {
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			if !strings.HasSuffix(name, "_test.go") {
				continue
			}
			data, readErr := os.ReadFile(filepath.Join(dir, name))
			if readErr != nil {
				continue
			}
			tests += len(testRe.FindAllString(string(data), -1))
		}
	}

	// Wired catalog entries: builder map keys in boot/catalog.go.
	catalogData, readErr := os.ReadFile(filepath.Join(root, "boot", "catalog.go"))
	if readErr != nil {
		return readErr
	}
	builderRe := regexp.MustCompile(`(?m)^\t"[^"]+": func\(deps CatalogDeps\) PluginSpec \{`)
	wired := len(builderRe.FindAllString(string(catalogData), -1))

	block := strings.Join([]string{
		fmt.Sprintf("- 工具链：%s / %s", runtime.Version(), runtime.GOOS+"/"+runtime.GOARCH),
		fmt.Sprintf("- 包：%d（含测试 %d，cmd 入口不计测试）", packages, withTests),
		fmt.Sprintf("- 行为测试函数：%d", tests),
		fmt.Sprintf("- catalog 接线：%d / 86（官方基线 cordis.patch.yml 名单为分母）", wired),
		fmt.Sprintf("- 生成时间：见 git log（由 `go run ./scripts/genstatus` 生成）"),
	}, "\n")

	statusPath := filepath.Join(root, "STATUS.md")
	statusData, readErr := os.ReadFile(statusPath)
	if readErr != nil {
		return readErr
	}
	const begin = "<!-- BEGIN GENERATED: do not edit -->"
	const end = "<!-- END GENERATED -->"
	content := string(statusData)
	start := strings.Index(content, begin)
	stop := strings.Index(content, end)
	if start < 0 || stop < 0 || stop < start {
		return fmt.Errorf("STATUS.md: generated markers not found")
	}
	updated := content[:start] + begin + "\n" + block + "\n" + content[stop:]
	return os.WriteFile(statusPath, []byte(updated), 0o644)
}

// packageDir resolves one package's directory via `go list -f {{.Dir}}`.
func packageDir(root, importPath string) (string, error) {
	out, err := execGo(root, "list", "-f", "{{.Dir}}", importPath)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// execGo runs the go tool with args rooted at dir.
func execGo(dir string, args ...string) (string, error) {
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("go %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

// The official-bundle disposition table — the single source of truth for
// rows the Go host intentionally does not mount. The composition guard
// (composition_guard_test.go) and the assembly mount loop both read it: a
// dispositioned row is exempted from the guard's drift report AND skipped
// at import with a warn, so the shipped bundles compose through every
// recorded no-port decision. Categories: frontend-domain (browser dist owns
// the row), T2-disposition (recorded no-port decision), T3-planned (Go port
// scheduled; removed from the table when the row lands).
package boot

// Dispositions maps an official entry name to its disposition category.
var Dispositions = map[string]string{
	// Frontend dist domain: the browser UI owns these rows; the Go host
	// catalog does not provide them (Owner ruling: frontend stays TS).
	"@deepseek-ai/dsh-client-runtime":             "frontend-domain",
	"@deepseek-ai/dsh-client-modules":             "frontend-domain",
	"@deepseek-ai/dsh-client-connection":          "frontend-domain",
	"@deepseek-ai/dsh-client-locale":              "frontend-domain",
	"@deepseek-ai/dsh-client-ui-theme":            "frontend-domain",
	"@deepseek-ai/dsh-client-ui-layout":           "frontend-domain",
	"@deepseek-ai/dsh-client-ui-sidebar":          "frontend-domain",
	"@deepseek-ai/dsh-client-ui-settings":         "frontend-domain",
	"@deepseek-ai/dsh-client-ui-settings-general": "frontend-domain",
	"@deepseek-ai/dsh-client-ui-models":           "frontend-domain",
	"@deepseek-ai/dsh-client-ui-model":            "frontend-domain",
	"@deepseek-ai/dsh-client-ui-conversation":     "frontend-domain",
	"@deepseek-ai/dsh-client-ui-tool":             "frontend-domain",
	"@deepseek-ai/dsh-client-ui-deliverables":     "frontend-domain",
	"@deepseek-ai/dsh-client-ui-workspace":        "frontend-domain",
	"@deepseek-ai/dsh-client-ui-slash":            "frontend-domain",
	"@deepseek-ai/dsh-client-ui-command":          "frontend-domain",
	"@deepseek-ai/dsh-client-ui-skill":            "frontend-domain",
	"@deepseek-ai/dsh-client-ui-subagent":         "frontend-domain",
	"@deepseek-ai/dsh-client-ui-goal":             "frontend-domain",
	"@deepseek-ai/dsh-client-ui-permission":       "frontend-domain",
	"@deepseek-ai/dsh-client-ui-agent-preset":     "frontend-domain",
	"@deepseek-ai/dsh-client-ui-plan":             "frontend-domain",
	"@deepseek-ai/dsh-client-ui-question":         "frontend-domain",
	"@deepseek-ai/dsh-client-ui-trajectory":       "frontend-domain",

	// T2 disposition: recorded no-port decisions (external CLI adapters
	// and loader-only machinery; the Go host has no JS/loader runtime).
	// These are skipped at import with a warn — see DECISIONS.
	"@deepseek-ai/dsh-subagent-codex":       "T2-disposition",
	"@deepseek-ai/dsh-subagent-claude-code": "T2-disposition",
	"@deepseek-ai/dsh-typert-loader":        "T2-disposition",
	"@deepseek-ai/dsh-llm-pi-ai":            "T2-disposition (external pi-ai SDK adapter; port on demand, ROADMAP record)",

	// T3 planned: Go port scheduled this migration round. Removed from
	// this table when the row's catalog entry lands.
	"@deepseek-ai/dsh-host-directory-picker-auto": "T3-planned (directory picker)",
	"@deepseek-ai/dsh-code-runtime-worker":        "T3-planned (code runtime; coderuntime seam ported)",
}

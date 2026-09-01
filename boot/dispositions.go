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
	"@deepseek-ai/dsh-client-runtime":                      "frontend-domain",
	"@deepseek-ai/dsh-client-modules":                      "frontend-domain",
	"@deepseek-ai/dsh-client-connection":                   "frontend-domain",
	"@deepseek-ai/dsh-client-locale":                       "frontend-domain",
	"@deepseek-ai/dsh-client-ui-theme":                     "frontend-domain",
	"@deepseek-ai/dsh-client-ui-layout":                    "frontend-domain",
	"@deepseek-ai/dsh-client-ui-sidebar":                   "frontend-domain",
	"@deepseek-ai/dsh-client-ui-settings":                  "frontend-domain",
	"@deepseek-ai/dsh-client-ui-settings-general":          "frontend-domain",
	"@deepseek-ai/dsh-client-ui-models":                    "frontend-domain",
	"@deepseek-ai/dsh-client-ui-model":                     "frontend-domain",
	"@deepseek-ai/dsh-client-ui-conversation":              "frontend-domain",
	"@deepseek-ai/dsh-client-ui-tool":                      "frontend-domain",
	"@deepseek-ai/dsh-client-ui-deliverables":              "frontend-domain",
	"@deepseek-ai/dsh-client-ui-workspace":                 "frontend-domain",
	"@deepseek-ai/dsh-client-ui-slash":                     "frontend-domain",
	"@deepseek-ai/dsh-client-ui-command":                   "frontend-domain",
	"@deepseek-ai/dsh-client-ui-skill":                     "frontend-domain",
	"@deepseek-ai/dsh-client-ui-subagent":                  "frontend-domain",
	"@deepseek-ai/dsh-client-ui-goal":                      "frontend-domain",
	"@deepseek-ai/dsh-client-ui-permission":                "frontend-domain",
	"@deepseek-ai/dsh-client-ui-agent-preset":              "frontend-domain",
	"@deepseek-ai/dsh-client-ui-plan":                      "frontend-domain",
	"@deepseek-ai/dsh-client-ui-question":                  "frontend-domain",
	"@deepseek-ai/dsh-client-ui-trajectory":                "frontend-domain",
	"@deepseek-ai/dsh-cordis-client-runner":                "frontend-domain",
	"@deepseek-ai/dsh-client-ui-approval":                  "frontend-domain",
	"@deepseek-ai/dsh-client-ui-attachment":                "frontend-domain",
	"@deepseek-ai/dsh-client-ui-brand-official":            "frontend-domain",
	"@deepseek-ai/dsh-client-ui-chat":                      "frontend-domain",
	"@deepseek-ai/dsh-client-ui-commands":                  "frontend-domain",
	"@deepseek-ai/dsh-client-ui-cordis":                    "frontend-domain",
	"@deepseek-ai/dsh-client-ui-input-trigger":             "frontend-domain",
	"@deepseek-ai/dsh-client-ui-jobs":                      "frontend-domain",
	"@deepseek-ai/dsh-client-ui-message-feedback":          "frontend-domain",
	"@deepseek-ai/dsh-client-ui-model-selection":           "frontend-domain",
	"@deepseek-ai/dsh-client-ui-permission-presets":        "frontend-domain",
	"@deepseek-ai/dsh-client-ui-reference":                 "frontend-domain",
	"@deepseek-ai/dsh-client-ui-renderer":                  "frontend-domain",
	"@deepseek-ai/dsh-client-ui-session":                   "frontend-domain",
	"@deepseek-ai/dsh-client-ui-settings-models":           "frontend-domain",
	"@deepseek-ai/dsh-client-ui-settings-plugin-inventory": "frontend-domain",
	"@deepseek-ai/dsh-client-ui-settings-plugins":          "frontend-domain",
	"@deepseek-ai/dsh-client-ui-user-questions":            "frontend-domain",
	"@deepseek-ai/dsh-client-ui-workflow-run":              "frontend-domain",
	"@deepseek-ai/dsh-client-hmr":                          "frontend-domain (client-side package; host cannot import)",

	// T2 disposition: recorded no-port decisions (external CLI adapters
	// and loader-only machinery; the Go host has no JS/loader runtime).
	// These are skipped at import with a warn — see DECISIONS.
	"@deepseek-ai/dsh-subagent-codex":                    "T2-disposition",
	"@deepseek-ai/dsh-subagent-claude-code":              "T2-disposition",
	"@deepseek-ai/dsh-typert-loader":                     "T2-disposition",
	"@deepseek-ai/dsh-llm-pi-ai":                         "T2-disposition (external pi-ai SDK adapter; port on demand, ROADMAP record)",
	"@deepseek-ai/dsh-cordis-host-runner":                "T2-disposition (host-side loader runner; no JS runtime)",
	"@deepseek-ai/dsh-host-plugin-inventory":             "T2-disposition (plugin inventory loader)",
	"@deepseek-ai/dsh-plugin-package-inventory-deepseek": "T2-disposition (npm package inventory; N-A record)",

	// T3 planned: Go port scheduled this migration round. Removed from
	// this table when the row's catalog entry lands.
	"@deepseek-ai/dsh-host-directory-picker-auto": "T3-planned (directory picker)",
	"@deepseek-ai/dsh-code-runtime-worker-thread": "T3-planned-skip (web-mode PTC code execution deferred; see DECISIONS)",
	"@deepseek-ai/dsh-session-reference":          "T3-planned (sessionreference ported; catalog row pending)",
	"@deepseek-ai/dsh-session-stats":              "T3-planned (sessionstats ported; catalog row pending)",
	"@deepseek-ai/dsh-session-log-export":         "T3-planned (sessionlog ported; catalog row pending)",
	"@deepseek-ai/dsh-session-turn-outline":       "T3-planned (session turn outline)",
	"@deepseek-ai/dsh-api-session-controller":     "T3-planned (api session controller)",
	"@deepseek-ai/dsh-api-settings-controller":    "T3-planned (api settings controller)",
	"@deepseek-ai/dsh-api-workspace-controller":   "T3-planned (api workspace controller)",
}

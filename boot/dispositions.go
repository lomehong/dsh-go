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
	"@deepseek-ai/dsh-client-file-upload":                  "frontend-domain (client half; the host upload wire — /api/session/uploadFileBinary + fileUploads remote — is NOT composed yet; r139 audit)",
	"@deepseek-ai/dsh-client-ui-schedule":                  "frontend-domain",
	"@deepseek-ai/cordis-plugin-hmr":                       "T2-disposition (upstream base yml disabled:true; node-specific HMR)",

	// T2 disposition: recorded no-port decisions (external CLI adapters
	// and loader-only machinery; the Go host has no JS/loader runtime).
	// These are skipped at import with a warn — see DECISIONS.
	"@deepseek-ai/dsh-subagent-codex":                    "T2-disposition",
	"@deepseek-ai/dsh-subagent-claude-code":              "T2-disposition",
	"@deepseek-ai/dsh-typert-loader":                     "T2-disposition",
	"@deepseek-ai/dsh-tool-workflow":                     "T2-disposition (model-facing JS workflow tool; Go workflow engine stays a library face)",
	"@deepseek-ai/dsh-workflow-worker-thread":            "T2-disposition (Node worker execution model; Go engine executes compiled scripts directly)",
	"@deepseek-ai/dsh-cordis-host-runner":                "T2-disposition (host-side loader runner; no JS runtime)",
	"@deepseek-ai/dsh-host-plugin-inventory":             "T2-disposition (plugin inventory loader)",
	"@deepseek-ai/dsh-plugin-package-inventory-deepseek": "T2-disposition (npm package inventory; N-A record)",

	// Invariant companion packages: the official dev-time observation
	// harness over production types (Go relies on the test suite instead —
	// recorded deviation, see the schedule/preset rows' invariant notes).
	"@deepseek-ai/dsh-invariants":           "T2-disposition (dev-time invariant harness; Go gates on tests)",
	"@deepseek-ai/dsh-agent/invariant":      "T2-disposition (invariant companion)",
	"@deepseek-ai/dsh-agent-loop/invariant": "T2-disposition (invariant companion)",
	"@deepseek-ai/dsh-scope/invariant":      "T2-disposition (invariant companion)",
	"@deepseek-ai/dsh-session/invariant":    "T2-disposition (invariant companion)",

	// T3 planned: Go port scheduled this migration round. Removed from
	// this table when the row's catalog entry lands. The r139 audit found
	// the acp/sdk/sdk-minimal profiles failing hard at import over these
	// rows; the dispositions make every selectable profile compose through
	// with warns instead.
	"@deepseek-ai/dsh-acp":                        "T3-planned (ACP provider surface; subagent provider packs)",
	"@deepseek-ai/dsh-acp-app":                    "T3-planned (ACP app startup over the ACP provider)",
	"@deepseek-ai/dsh-sdk-minimal":                "T3-planned (the sdk-minimal bundle's own startup row)",
	"@deepseek-ai/dsh-fs-local":                   "T3-planned (bare local fs row — Go ships fs through dsh-fs-sandbox)",
	"@deepseek-ai/dsh-terminal":                   "T3-planned (terminal/pty primitives — documented deferral)",
	"@deepseek-ai/dsh-terminal-bash":              "T3-planned (terminal bash executor)",
	"@deepseek-ai/dsh-tool-bash-persistent":       "T3-planned (persistent shell tool family)",
	"@deepseek-ai/dsh-tool-pwsh-persistent":       "T3-planned (persistent shell tool family)",
	"@deepseek-ai/dsh-host-directory-picker-auto": "T2-disposition (auto chooser row absent; its EFFECTIVE browse outcome is composed natively: gateway DirectoryPickerController serves list/createDirectory, webhost serves the browse client face — native OS-chooser face stays unported, launcher capability)",
	"@deepseek-ai/dsh-code-runtime-worker-thread": "T3-planned-skip (web-mode PTC code execution deferred; see DECISIONS)",
	"@deepseek-ai/dsh-session-stats":              "T3-planned (sessionstats ported; catalog row pending)",
	"@deepseek-ai/dsh-session-log-export":         "frontend-domain (browser /export command + download dialog; the shared archive helpers port as sessionlog)",
	// Delivered under Go packaging: the api-controller rows' Remote
	// controllers live in the gateway package and compose through the
	// apiGateway bundle row (r96d/e + r105-r112); the turn-outline
	// projection unit ports as sessionturnoutline and registers inside the
	// apiGateway composition.
	"@deepseek-ai/dsh-session-turn-outline":     "T2-resolved (sessionturnoutline unit, apiGateway-composed)",
	"@deepseek-ai/dsh-api-session-controller":   "T2-resolved (gateway session controller)",
	"@deepseek-ai/dsh-api-settings-controller":  "T2-resolved (gateway settings controller)",
	"@deepseek-ai/dsh-api-workspace-controller": "T2-resolved (gateway workspace controller)",
}

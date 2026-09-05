package llm

// The installed multi-provider catalog: the provider ids the official pi-ai
// twin offers on the Models page (official directoryEntries over the pi-ai
// builtinProviders catalog). Routes configured here need a matching wire
// adapter at dispatch — the Go host currently ships the DeepSeek adapter
// family; other apis dispatch NO_ADAPTER until the multi-provider adapter
// round lands (ROADMAP).

// PiAiCatalogProviders is the stable catalog provider id list, in the order
// the official Models page renders them (catalog declaration order).
var PiAiCatalogProviders = []string{
	"openai",
	"anthropic",
	"google",
	"google-vertex",
	"azure-openai-responses",
	"amazon-bedrock",
	"openrouter",
	"groq",
	"mistral",
	"xai",
	"deepseek",
	"together",
	"fireworks",
	"cerebras",
	"baseten",
	"nvidia",
	"minimax",
	"minimax-cn",
	"moonshotai",
	"moonshotai-cn",
	"kimi-coding",
	"zai",
	"zai-coding-cn",
	"qwen-token-plan-cn",
	"qwen-token-plan-individual",
	"qwen-token-plan",
	"xiaomi",
	"xiaomi-token-plan-cn",
	"xiaomi-token-plan-sgp",
	"xiaomi-token-plan-ams",
	"ant-ling",
	"opencode",
	"opencode-go",
	"openai-codex",
	"openrouter-images",
	"cloudflare-ai-gateway",
	"cloudflare-workers-ai",
	"cloudflare-stream",
	"cloudflare-auth",
	"radius",
	"radius-config",
}

// PiAiCatalogEntries builds the configurable-provider entries the Models
// page renders: one per catalog provider, each backed by the llm-pi-ai
// settings namespace at providers/<id> (official directoryEntries).
func PiAiCatalogEntries() []ConfigurableProvider {
	entries := make([]ConfigurableProvider, 0, len(PiAiCatalogProviders))
	for _, provider := range PiAiCatalogProviders {
		entries = append(entries, ConfigurableProvider{
			Provider:     provider,
			DisplayName:  provider,
			SettingsNs:   "llm-pi-ai",
			SettingsPath: []string{"providers", provider},
		})
	}
	return entries
}

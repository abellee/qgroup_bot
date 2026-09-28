package admin

import "qgroup-bot/internal/store"

// providerOption is one entry of the 提供商 dropdown, served as JSON so the app
// cannot offer a value the store would reject.
type providerOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

var providerLabels = []providerOption{
	{store.ProviderOpenAI, "OpenAI 兼容"},
	{store.ProviderAnthropic, "Anthropic"},
	{store.ProviderGemini, "Gemini"},
}

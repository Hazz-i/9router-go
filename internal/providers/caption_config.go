package providers

import "slices"

// LiveCaptionConfig is one provider's live-caption declaration, ported from the
// upstream registry's liveCaptionConfig (registry/{deepgram,elevenlabs,
// selfhosted-stt}.js). Upstream's handler hardcodes the dial URLs and treats
// these fields as declarative metadata, so they stay data here too — the
// caption handler keys its transport off Format, never off a model id.
type LiveCaptionConfig struct {
	DefaultModel string
	WsURL        string // absolute ws(s) URL for the socket backends
	BaseURL      string // selfhosted-stt only
	AuthType     string
	AuthHeader   string // "token" | "xi-api-key" | "bearer"
	Format       string // "deepgram" | "elevenlabs-scribe" | "openai"
}

// LiveCaptionConfigs is keyed by CANONICAL provider id. Gemini deliberately has
// no entry: upstream gates it in via serviceKinds "stt" alone (its branch lives
// in the handler's Gemini Live dispatch), so any faithful gate must accept a
// provider without a config here.
var LiveCaptionConfigs = map[string]*LiveCaptionConfig{
	"deepgram":   {WsURL: "wss://api.deepgram.com/v1/listen", AuthType: "apikey", AuthHeader: "token", Format: "deepgram"},
	"elevenlabs": {WsURL: "wss://api.elevenlabs.io/v1/speech-to-text/realtime", DefaultModel: "scribe_v2_realtime", AuthType: "apikey", AuthHeader: "xi-api-key", Format: "elevenlabs-scribe"},
	"selfhosted-stt": {
		BaseURL:    "http://localhost:8080/v1/audio/transcriptions",
		AuthType:   "apikey",
		AuthHeader: "bearer",
		Format:     "openai",
	},
}

// GetLiveCaptionConfig resolves aliases through the existing ProviderAliasMap
// and returns nil when the provider declares no caption config.
func GetLiveCaptionConfig(provider string) *LiveCaptionConfig {
	return LiveCaptionConfigs[ResolveAlias(provider)]
}

// serviceKinds ports the upstream registry's provider-level serviceKinds for
// the providers that declare any caption/realtime-relevant kind. Upstream's
// capability gates read this field (never model-id patterns), so the gate
// below is data-driven: a provider gains/loses live caption by a registry
// edit, not a code change. Providers absent here declare no such kind.
var serviceKinds = map[string][]string{
	"deepgram":       {"stt", "liveCaption", "realtime"},
	"elevenlabs":     {"tts", "stt", "liveCaption"},
	"gemini":         {"llm", "embedding", "image", "imageToText", "webSearch", "tts", "stt", "realtime"},
	"groq":           {"llm", "imageToText", "stt"},
	"huggingface":    {"image", "stt"},
	"openai":         {"llm", "embedding", "tts", "stt", "image", "imageToText", "webSearch"},
	"assemblyai":     {"stt"},
	"selfhosted-stt": {"stt", "liveCaption"},
}

// SupportsLiveCaption reproduces upstream's gate: the provider passes when its
// serviceKinds contain "liveCaption" OR "stt" (registry/*.js serviceKinds).
// Having no LiveCaptionConfigs entry does not fail the gate — such providers
// fall to the selfhosted branch in the handler, exactly like upstream.
func SupportsLiveCaption(provider string) bool {
	kinds, ok := serviceKinds[ResolveAlias(provider)]
	if !ok {
		return false
	}
	return slices.Contains(kinds, "liveCaption") || slices.Contains(kinds, "stt")
}

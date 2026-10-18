package providers

import (
	"fmt"
	"strings"
)

// RealtimeConfig is one provider's realtime-voice declaration, ported
// field-for-field from the upstream registry's realtimeConfig
// (open-sse/providers/registry/{gemini,deepgram}.js). The Deepgram handler
// embeds Listen/Speak/Think fragments into its Settings frame; Gemini reads
// only the connection fields — its bidiUrl rides WsURL and its ?key= query
// parameter rides AuthQueryParam, the two names the frozen contract reserves.
type RealtimeConfig struct {
	DefaultModel    string
	WsURL           string // absolute ws(s) URL; for Gemini this is bidiUrl
	AuthType        string // "apikey"
	AuthHeader      string // "Authorization"
	AuthScheme      string // "Token"
	AuthQueryParam  string // "key" (gemini only)
	AudioInRate     int    // default 16000 when 0
	AudioOutRate    int    // default 24000 when 0
	KeepAliveMs     int    // default 5000 when 0
	SupportedVoices []string
	Listen          *RealtimeChoiceSet // nil = provider takes no listen override
	Speak           *RealtimeChoiceSet
	Think           *RealtimeChoiceSet
}

// RealtimeChoiceSet is one of listen/speak/think: the no-override default
// `provider` fragment plus the provider groups an override may name. Keeping
// them as data (not code) is what keeps a new voice/LLM entry a registry edit.
type RealtimeChoiceSet struct {
	Provider  map[string]any        // the default `provider` fragment
	Providers []RealtimeChoiceGroup // groups keyed by upstream provider type
}

// RealtimeChoiceGroup mirrors one upstream entry of realtimeConfig.<kind>.providers.
type RealtimeChoiceGroup struct {
	Type        string           // "deepgram", "eleven_labs", "open_ai", ...
	Choices     []RealtimeChoice // upstream `models` (listen/think) or `voices` (speak)
	CustomVoice map[string]any   // speak only; may contain the literal "@id" placeholder
}

// RealtimeChoice is one pickable id; Settings is nil for entries whose agent
// fragment is exactly the default {provider: {type, model}} shape.
type RealtimeChoice struct {
	ID       string         // the `<id>` of `<type>/<id>`; may contain slashes
	Settings map[string]any // nil ⇒ default fragment {"provider": {"type": Type, "model": ID}}
}

// Resolve reproduces upstream deepgramAdapter resolveOverride for
// ?listen/?think/?speak=<type>/<id>, including the `custom:<id>` passthrough
// and the "@id" substitution in a speak group's CustomVoice template. It
// returns the `provider`-carrying settings fragment the handler embeds in the
// Deepgram Settings frame, or nil when raw is empty (no override).
//
// A custom id is deep-copied out of the shared template so concurrent sessions
// can never see each other's substitution. On an unknown type or id it returns
// an error whose message is exactly:
//
//	Unknown realtime <kind> choice "<raw>". Valid options: <opt1>, <opt2>, ...
//
// so the handler can fail BEFORE any upstream dial.
func (s *RealtimeChoiceSet) Resolve(kind, raw string) (map[string]any, error) {
	if raw == "" {
		return nil, nil
	}
	prov, id, _ := strings.Cut(raw, "/")
	group := s.group(prov)
	if group == nil {
		return nil, s.unknownChoiceError(kind, raw)
	}
	if strings.HasPrefix(id, "custom:") {
		rawID := strings.TrimPrefix(id, "custom:")
		if rawID == "" {
			return nil, fmt.Errorf("Missing custom %s id for provider \"%s\" (use %s/custom:<id>).", kind, prov, prov)
		}
		if group.CustomVoice != nil {
			sub, ok := substituteCustomID(group.CustomVoice, rawID).(map[string]any)
			if !ok {
				return nil, fmt.Errorf("realtime %s customVoice template for provider %q is not an object", kind, prov)
			}
			return sub, nil
		}
		// think/listen have no template: the raw id rides a plain {type, model}.
		return map[string]any{"provider": map[string]any{"type": group.Type, "model": rawID}}, nil
	}
	for _, c := range group.Choices {
		if c.ID == id {
			if c.Settings != nil {
				return c.Settings, nil
			}
			return map[string]any{"provider": map[string]any{"type": group.Type, "model": id}}, nil
		}
	}
	return nil, s.unknownChoiceError(kind, raw)
}

func (s *RealtimeChoiceSet) group(typ string) *RealtimeChoiceGroup {
	for i := range s.Providers {
		if s.Providers[i].Type == typ {
			return &s.Providers[i]
		}
	}
	return nil
}

// unknownChoiceError reproduces the upstream message character-for-character:
// the valid list is every id of every group as "<type>/<id>", joined ", " —
// the id itself may contain slashes (groq/openai/gpt-oss-20b), so the list is
// the only unambiguous spelling of the accepted choices.
func (s *RealtimeChoiceSet) unknownChoiceError(kind, raw string) error {
	opts := make([]string, 0, 64)
	for _, g := range s.Providers {
		for _, c := range g.Choices {
			opts = append(opts, g.Type+"/"+c.ID)
		}
	}
	return fmt.Errorf("Unknown realtime %s choice \"%s\". Valid options: %s", kind, raw, strings.Join(opts, ", "))
}

// substituteCustomID replaces every string equal to "@id" with rawID in a fresh
// copy of v, mirroring the upstream deep-copy so the shared template is never
// mutated between sessions.
func substituteCustomID(v any, rawID string) any {
	switch t := v.(type) {
	case string:
		if t == "@id" {
			return rawID
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = substituteCustomID(val, rawID)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = substituteCustomID(val, rawID)
		}
		return out
	default:
		return v
	}
}

// dgVersionedVoice builds the fragment for Deepgram voices that ride an
// explicit provider version (aura v1 / flux v2 need the version+model pair);
// plain aura-2 ids need no settings at all.
func dgVersionedVoice(id, version string) RealtimeChoice {
	return RealtimeChoice{ID: id, Settings: map[string]any{
		"provider": map[string]any{"type": "deepgram", "version": version, "model": id},
	}}
}

// RealtimeConfigs is keyed by CANONICAL provider id. Data ported verbatim from
// registry/deepgram.js realtimeConfig (listen/speak/think groups) and
// registry/gemini.js realtimeConfig (connection fields only — Gemini takes no
// listen/think/speak overrides).
var RealtimeConfigs = map[string]*RealtimeConfig{
	"gemini": {
		DefaultModel:    "gemini-3.8-live",
		WsURL:           "wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent",
		AuthType:        "apikey",
		AuthQueryParam:  "key",
		AudioInRate:     16000,
		AudioOutRate:    24000,
		SupportedVoices: []string{"Puck", "Charon", "Kore", "Fenrir", "Aoede"},
	},
	"deepgram": {
		DefaultModel: "voice-agent",
		WsURL:        "wss://agent.deepgram.com/v1/agent/converse",
		AuthType:     "apikey",
		AuthHeader:   "Authorization",
		AuthScheme:   "Token",
		AudioInRate:  16000,
		AudioOutRate: 24000,
		KeepAliveMs:  5000,
		Listen: &RealtimeChoiceSet{
			Provider: map[string]any{"type": "deepgram", "model": "nova-3"},
			Providers: []RealtimeChoiceGroup{{
				Type: "deepgram",
				Choices: []RealtimeChoice{
					{ID: "nova-3"}, {ID: "nova-2"},
					// Flux STT runs on provider version v2 (docs: version+model pair).
					{ID: "flux-general-en", Settings: map[string]any{"provider": map[string]any{"type": "deepgram", "version": "v2", "model": "flux-general-en"}}},
					{ID: "flux-general-multi", Settings: map[string]any{"provider": map[string]any{"type": "deepgram", "version": "v2", "model": "flux-general-multi"}}},
				},
			}},
		},
		Speak: &RealtimeChoiceSet{
			Provider: map[string]any{"type": "deepgram", "model": "aura-2-thalia-en"},
			Providers: []RealtimeChoiceGroup{
				{
					Type: "deepgram",
					Choices: []RealtimeChoice{
						// Aura-2 English (docs/tts-models.md "Aura-2 All Available English Voices").
						{ID: "aura-2-thalia-en"}, {ID: "aura-2-andromeda-en"}, {ID: "aura-2-helena-en"}, {ID: "aura-2-apollo-en"},
						{ID: "aura-2-arcas-en"}, {ID: "aura-2-aries-en"}, {ID: "aura-2-amalthea-en"}, {ID: "aura-2-asteria-en"},
						{ID: "aura-2-athena-en"}, {ID: "aura-2-atlas-en"}, {ID: "aura-2-aurora-en"}, {ID: "aura-2-callista-en"},
						{ID: "aura-2-cora-en"}, {ID: "aura-2-cordelia-en"}, {ID: "aura-2-delia-en"}, {ID: "aura-2-draco-en"},
						{ID: "aura-2-electra-en"}, {ID: "aura-2-harmonia-en"}, {ID: "aura-2-hera-en"}, {ID: "aura-2-hermes-en"},
						{ID: "aura-2-hyperion-en"}, {ID: "aura-2-iris-en"}, {ID: "aura-2-janus-en"}, {ID: "aura-2-juno-en"},
						{ID: "aura-2-jupiter-en"}, {ID: "aura-2-luna-en"}, {ID: "aura-2-mars-en"}, {ID: "aura-2-minerva-en"},
						{ID: "aura-2-neptune-en"}, {ID: "aura-2-odysseus-en"}, {ID: "aura-2-ophelia-en"}, {ID: "aura-2-orion-en"},
						{ID: "aura-2-orpheus-en"}, {ID: "aura-2-pandora-en"}, {ID: "aura-2-phoebe-en"}, {ID: "aura-2-pluto-en"},
						{ID: "aura-2-saturn-en"}, {ID: "aura-2-selene-en"}, {ID: "aura-2-theia-en"}, {ID: "aura-2-vesta-en"},
						{ID: "aura-2-zeus-en"},
						// Aura v1 English (docs/tts-models.md "Aura (v1) English Voices").
						dgVersionedVoice("aura-asteria-en", "v1"), dgVersionedVoice("aura-luna-en", "v1"),
						dgVersionedVoice("aura-stella-en", "v1"), dgVersionedVoice("aura-athena-en", "v1"),
						dgVersionedVoice("aura-hera-en", "v1"), dgVersionedVoice("aura-orion-en", "v1"),
						dgVersionedVoice("aura-arcas-en", "v1"), dgVersionedVoice("aura-perseus-en", "v1"),
						dgVersionedVoice("aura-angus-en", "v1"), dgVersionedVoice("aura-orpheus-en", "v1"),
						dgVersionedVoice("aura-helios-en", "v1"), dgVersionedVoice("aura-zeus-en", "v1"),
						// Flux TTS voice agents (docs/flux-tts/voices.md), speaker version v2.
						dgVersionedVoice("flux-hannah-en", "v2"), dgVersionedVoice("flux-kit-en", "v2"),
						dgVersionedVoice("flux-alexis-en", "v2"), dgVersionedVoice("flux-cliff-en", "v2"),
						dgVersionedVoice("flux-sienna-en", "v2"), dgVersionedVoice("flux-cole-en", "v2"),
						dgVersionedVoice("flux-brooke-en", "v2"), dgVersionedVoice("flux-colin-en", "v2"),
						dgVersionedVoice("flux-gemma-en", "v2"), dgVersionedVoice("flux-haley-en", "v2"),
						dgVersionedVoice("flux-heather-en", "v2"), dgVersionedVoice("flux-miles-en", "v2"),
						dgVersionedVoice("flux-sean-en", "v2"),
					},
					CustomVoice: map[string]any{"provider": map[string]any{"type": "deepgram", "model": "@id"}},
				},
				{
					// Prebuilt premade voice ids (Jessica/Daniel); both support eleven_multilingual_v2.
					Type: "eleven_labs",
					Choices: []RealtimeChoice{
						{ID: "cgSgspJ2msm6clMCkdW9", Settings: map[string]any{"provider": map[string]any{"type": "eleven_labs", "model_id": "eleven_multilingual_v2", "voice_id": "cgSgspJ2msm6clMCkdW9"}}},
						{ID: "onwK4e9ZLuTAKqWW03F9", Settings: map[string]any{"provider": map[string]any{"type": "eleven_labs", "model_id": "eleven_multilingual_v2", "voice_id": "onwK4e9ZLuTAKqWW03F9"}}},
					},
					CustomVoice: map[string]any{"provider": map[string]any{"type": "eleven_labs", "model_id": "eleven_multilingual_v2", "voice_id": "@id"}},
				},
				{
					// Cartesia voice id (sonic-2) as documented on Deepgram's TTS page.
					Type: "cartesia",
					Choices: []RealtimeChoice{
						{ID: "a167e0f3-df7e-4d52-a9c3-f949145efdab", Settings: map[string]any{"provider": map[string]any{"type": "cartesia", "model_id": "sonic-2", "voice": map[string]any{"mode": "id", "id": "a167e0f3-df7e-4d52-a9c3-f949145efdab"}}}},
					},
					CustomVoice: map[string]any{"provider": map[string]any{"type": "cartesia", "model_id": "sonic-2", "voice": map[string]any{"mode": "id", "id": "@id"}}},
				},
				{
					// OpenAI tts-1 voices; ids keep the <model>/<voice> token so the URL
					// form matches Deepgram's own examples (speak=open_ai/tts-1/alloy).
					Type: "open_ai",
					Choices: []RealtimeChoice{
						{ID: "tts-1/alloy", Settings: map[string]any{"provider": map[string]any{"type": "open_ai", "model": "tts-1", "voice": "alloy"}}},
						{ID: "tts-1/echo", Settings: map[string]any{"provider": map[string]any{"type": "open_ai", "model": "tts-1", "voice": "echo"}}},
						{ID: "tts-1/fable", Settings: map[string]any{"provider": map[string]any{"type": "open_ai", "model": "tts-1", "voice": "fable"}}},
						{ID: "tts-1/onyx", Settings: map[string]any{"provider": map[string]any{"type": "open_ai", "model": "tts-1", "voice": "onyx"}}},
						{ID: "tts-1/nova", Settings: map[string]any{"provider": map[string]any{"type": "open_ai", "model": "tts-1", "voice": "nova"}}},
						{ID: "tts-1/shimmer", Settings: map[string]any{"provider": map[string]any{"type": "open_ai", "model": "tts-1", "voice": "shimmer"}}},
						{ID: "tts-1-hd/alloy", Settings: map[string]any{"provider": map[string]any{"type": "open_ai", "model": "tts-1-hd", "voice": "alloy"}}},
					},
					CustomVoice: map[string]any{"provider": map[string]any{"type": "open_ai", "model": "tts-1", "voice": "@id"}},
				},
				{
					// AWS Polly en-US standard-engine voices (Deepgram requires explicit engine).
					Type: "aws_polly",
					Choices: []RealtimeChoice{
						{ID: "Joanna", Settings: map[string]any{"provider": map[string]any{"type": "aws_polly", "voice": "Joanna", "engine": "standard", "language_code": "en-US"}}},
						{ID: "Matthew", Settings: map[string]any{"provider": map[string]any{"type": "aws_polly", "voice": "Matthew", "engine": "standard", "language_code": "en-US"}}},
					},
					CustomVoice: map[string]any{"provider": map[string]any{"type": "aws_polly", "voice": "@id", "engine": "standard", "language_code": "en-US"}},
				},
			},
		},
		Think: &RealtimeChoiceSet{
			Provider: map[string]any{"type": "google", "model": "gemini-3.1-flash-lite"},
			Providers: []RealtimeChoiceGroup{
				{
					Type: "open_ai",
					Choices: []RealtimeChoice{
						{ID: "gpt-4.1"}, {ID: "gpt-4.1-mini"}, {ID: "gpt-4.1-nano"}, {ID: "gpt-4o"}, {ID: "gpt-4o-mini"},
						{ID: "gpt-5"}, {ID: "gpt-5-mini"}, {ID: "gpt-5-nano"}, {ID: "gpt-5.1"}, {ID: "gpt-5.2"}, {ID: "gpt-5.4"},
						{ID: "gpt-5.4-mini"}, {ID: "gpt-5.4-nano"}, {ID: "gpt-5.5"}, {ID: "gpt-5.6-luna"}, {ID: "gpt-5.6-terra"},
						{ID: "gpt-6-luna"},
					},
				},
				{
					Type: "anthropic",
					Choices: []RealtimeChoice{
						{ID: "claude-haiku-4-5"}, {ID: "claude-haiku-5-5"}, {ID: "claude-sonnet-4-5"},
						{ID: "claude-sonnet-4-6"}, {ID: "claude-sonnet-5"}, {ID: "claude-sonnet-5-5"},
					},
				},
				{
					Type: "google",
					Choices: []RealtimeChoice{
						{ID: "gemini-2.5-flash"}, {ID: "gemini-2.5-flash-lite"}, {ID: "gemini-3-flash-preview"},
						{ID: "gemini-3.1-flash-lite"}, {ID: "gemini-3.1-pro-preview"}, {ID: "gemini-3.5-flash"},
						{ID: "gemini-3.8-flash"},
					},
				},
				// groq/nvidia entries also need the endpoint hop upstream; the
				// fragment is still the plain {type, model} pair.
				{Type: "groq", Choices: []RealtimeChoice{{ID: "openai/gpt-oss-20b"}}},
				{Type: "nvidia", Choices: []RealtimeChoice{{ID: "nvidia/nemotron-3.5-lightning-30b-a3b"}}},
			},
		},
	},
}

// GetRealtimeConfig resolves aliases through the existing ProviderAliasMap (dg,
// el, ...) and returns nil when the provider has no realtime voice config.
func GetRealtimeConfig(provider string) *RealtimeConfig {
	return RealtimeConfigs[ResolveAlias(provider)]
}

// SupportsRealtimeVoice reports whether the provider carries a realtimeConfig.
func SupportsRealtimeVoice(provider string) bool {
	return GetRealtimeConfig(provider) != nil
}

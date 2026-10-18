package providers

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Ported assertions from upstream tests/unit/realtime-adapters.test.js
// (Deepgram realtime voice) and the live-caption contract
// (src/sse/handlers/liveCaptionWs.js provider gate).

func realTimeKindSets(cfg *RealtimeConfig) map[string]*RealtimeChoiceSet {
	return map[string]*RealtimeChoiceSet{"listen": cfg.Listen, "speak": cfg.Speak, "think": cfg.Think}
}

// TestRealtimeRegistrySelfConsistent is upstream test 5: for each of
// listen/speak/think the default provider.model id exists inside the group
// whose type matches, and every speak group's customVoice template carries
// the "@id" placeholder.
func TestRealtimeRegistrySelfConsistent(t *testing.T) {
	cfg := RealtimeConfigs["deepgram"]
	if cfg == nil {
		t.Fatal("RealtimeConfigs has no deepgram entry")
	}
	for kind, set := range realTimeKindSets(cfg) {
		if set == nil {
			t.Fatalf("deepgram realtimeConfig.%s is nil", kind)
		}
		t.Run("default "+kind+" id exists in its group", func(t *testing.T) {
			typ, _ := set.Provider["type"].(string)
			model, _ := set.Provider["model"].(string)
			var group *RealtimeChoiceGroup
			for i := range set.Providers {
				if set.Providers[i].Type == typ {
					group = &set.Providers[i]
				}
			}
			if group == nil {
				t.Fatalf("no group of type %q for %s default", typ, kind)
			}
			found := false
			for _, c := range group.Choices {
				if c.ID == model {
					found = true
				}
			}
			if !found {
				t.Errorf("%s default model %q not listed in the %q group", kind, model, typ)
			}
		})
	}
	t.Run("every speak group customVoice carries @id", func(t *testing.T) {
		for _, g := range cfg.Speak.Providers {
			raw, err := json.Marshal(g.CustomVoice)
			if err != nil {
				t.Fatalf("marshal customVoice %s: %v", g.Type, err)
			}
			if !strings.Contains(string(raw), "@id") {
				t.Errorf("speak group %q customVoice lacks \"@id\": %s", g.Type, raw)
			}
		}
	})
	t.Run("ported entry counts match the upstream registry", func(t *testing.T) {
		counts := map[string]map[string]int{
			"listen": {"deepgram": 4},
			"speak":  {"deepgram": 66, "eleven_labs": 2, "cartesia": 1, "open_ai": 7, "aws_polly": 2},
			"think":  {"open_ai": 17, "anthropic": 6, "google": 7, "groq": 1, "nvidia": 1},
		}
		for kind, want := range counts {
			got := map[string]int{}
			for _, g := range realTimeKindSets(cfg)[kind].Providers {
				got[g.Type] = len(g.Choices)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s group counts = %v, want %v", kind, got, want)
			}
		}
	})
}

func TestRealtimeResolveOverrides(t *testing.T) {
	dg := RealtimeConfigs["deepgram"]
	cases := []struct {
		name string
		kind string
		raw  string
		want map[string]any
	}{
		{
			name: "listen override rides the version v2 flux fragment",
			kind: "listen", raw: "deepgram/flux-general-multi",
			want: map[string]any{"provider": map[string]any{"type": "deepgram", "version": "v2", "model": "flux-general-multi"}},
		},
		{
			name: "think override without settings uses the plain fragment",
			kind: "think", raw: "google/gemini-2.5-flash",
			want: map[string]any{"provider": map[string]any{"type": "google", "model": "gemini-2.5-flash"}},
		},
		{
			name: "speak rest-of-slash id parses to model+voice",
			kind: "speak", raw: "open_ai/tts-1/alloy",
			want: map[string]any{"provider": map[string]any{"type": "open_ai", "model": "tts-1", "voice": "alloy"}},
		},
		{
			name: "groq think keeps the slashed model id",
			kind: "think", raw: "groq/openai/gpt-oss-20b",
			want: map[string]any{"provider": map[string]any{"type": "groq", "model": "openai/gpt-oss-20b"}},
		},
		{
			name: "eleven_labs speak uses model_id/voice_id shape",
			kind: "speak", raw: "eleven_labs/cgSgspJ2msm6clMCkdW9",
			want: map[string]any{"provider": map[string]any{"type": "eleven_labs", "model_id": "eleven_multilingual_v2", "voice_id": "cgSgspJ2msm6clMCkdW9"}},
		},
		{
			name: "custom speak id substitutes into the eleven_labs template",
			kind: "speak", raw: "eleven_labs/custom:my-cloned-voice",
			want: map[string]any{"provider": map[string]any{"type": "eleven_labs", "model_id": "eleven_multilingual_v2", "voice_id": "my-cloned-voice"}},
		},
		{
			name: "custom think id rides a plain fragment",
			kind: "think", raw: "open_ai/custom:ft-gpt-5.5-myorg",
			want: map[string]any{"provider": map[string]any{"type": "open_ai", "model": "ft-gpt-5.5-myorg"}},
		},
		{
			name: "custom deepgram speak id substitutes into its template",
			kind: "speak", raw: "deepgram/custom:my-aura",
			want: map[string]any{"provider": map[string]any{"type": "deepgram", "model": "my-aura"}},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, err := realTimeKindSets(dg)[tt.kind].Resolve(tt.kind, tt.raw)
			if err != nil {
				t.Fatalf("Resolve(%s, %q) error = %v", tt.kind, tt.raw, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Resolve(%s, %q) = %v, want %v", tt.kind, tt.raw, got, tt.want)
			}
		})
	}
	t.Run("empty override resolves to nil so defaults survive", func(t *testing.T) {
		got, err := dg.Listen.Resolve("listen", "")
		if got != nil || err != nil {
			t.Errorf("Resolve(\"\") = (%v, %v), want (nil, nil)", got, err)
		}
	})
	t.Run("custom substitution never mutates the shared template", func(t *testing.T) {
		el := dg.Speak.Providers[1]
		if _, err := dg.Speak.Resolve("speak", "eleven_labs/custom:voice-a"); err != nil {
			t.Fatalf("first custom resolve: %v", err)
		}
		got, err := dg.Speak.Resolve("speak", "eleven_labs/custom:voice-b")
		if err != nil {
			t.Fatalf("second custom resolve: %v", err)
		}
		if !strings.Contains(el.CustomVoice["provider"].(map[string]any)["voice_id"].(string), "@id") {
			t.Error("shared customVoice template was mutated by a previous resolve")
		}
		want := map[string]any{"provider": map[string]any{"type": "eleven_labs", "model_id": "eleven_multilingual_v2", "voice_id": "voice-b"}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("second resolve = %v, want %v", got, want)
		}
	})
}

func TestRealtimeResolveUnknown(t *testing.T) {
	dg := RealtimeConfigs["deepgram"]
	cases := []struct {
		name     string
		kind     string
		raw      string
		wantIn   []string // substrings the message must contain
		wantNone []string // substrings the message must NOT contain
	}{
		{
			name: "unknown provider type fails with the full valid list",
			kind: "speak", raw: "minimax/custom:my-voice",
			wantIn: []string{
				`Unknown realtime speak choice "minimax/custom:my-voice". Valid options: `,
				"deepgram/aura-2-thalia-en",
				"eleven_labs/cgSgspJ2msm6clMCkdW9",
				"open_ai/tts-1/alloy",
			},
			wantNone: []string{"google/gemini-3.1-flash-lite"},
		},
		{
			name: "unknown id in a known group fails with the full valid list",
			kind: "think", raw: "google/gpt-9-turbo-nova",
			wantIn: []string{
				`Unknown realtime think choice "google/gpt-9-turbo-nova". Valid options: `,
				"open_ai/gpt-4.1",
				"google/gemini-3.1-flash-lite",
				"groq/openai/gpt-oss-20b",
			},
			wantNone: []string{"google/gpt-9-turbo-nova, "},
		},
		{
			name: "empty custom id names the fix",
			kind: "speak", raw: "deepgram/custom:",
			wantIn: []string{`Missing custom speak id for provider "deepgram" (use deepgram/custom:<id>).`},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := realTimeKindSets(dg)[tt.kind].Resolve(tt.kind, tt.raw)
			if err == nil {
				t.Fatalf("Resolve(%s, %q) = nil error, want unknown-choice error", tt.kind, tt.raw)
			}
			for _, want := range tt.wantIn {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q lacks %q", err.Error(), want)
				}
			}
			for _, bad := range tt.wantNone {
				if strings.Contains(err.Error(), bad) {
					t.Errorf("error %q must not contain %q", err.Error(), bad)
				}
			}
		})
	}
}

func TestGetRealtimeConfig(t *testing.T) {
	t.Run("gemini carries connection fields only", func(t *testing.T) {
		cfg := GetRealtimeConfig("gemini")
		if cfg == nil {
			t.Fatal("GetRealtimeConfig(gemini) = nil")
		}
		if cfg.DefaultModel != "gemini-3.8-live" {
			t.Errorf("DefaultModel = %q", cfg.DefaultModel)
		}
		if !strings.HasPrefix(cfg.WsURL, "wss://generativelanguage.googleapis.com/ws/") {
			t.Errorf("WsURL = %q, want the bidi URL", cfg.WsURL)
		}
		if cfg.AuthQueryParam != "key" || cfg.AudioInRate != 16000 || cfg.AudioOutRate != 24000 {
			t.Errorf("auth/rates = %q/%d/%d", cfg.AuthQueryParam, cfg.AudioInRate, cfg.AudioOutRate)
		}
		want := []string{"Puck", "Charon", "Kore", "Fenrir", "Aoede"}
		if !reflect.DeepEqual(cfg.SupportedVoices, want) {
			t.Errorf("SupportedVoices = %v, want %v", cfg.SupportedVoices, want)
		}
		if cfg.Listen != nil || cfg.Speak != nil || cfg.Think != nil {
			t.Error("gemini must take no listen/speak/think overrides")
		}
	})
	t.Run("deepgram resolves via the dg alias to the same entry", func(t *testing.T) {
		canonical := GetRealtimeConfig("deepgram")
		for _, id := range []string{"dg", "deepgram"} {
			if got := GetRealtimeConfig(id); got != canonical {
				t.Errorf("GetRealtimeConfig(%q) is not the canonical deepgram entry", id)
			}
		}
		if canonical.AuthHeader != "Authorization" || canonical.AuthScheme != "Token" || canonical.KeepAliveMs != 5000 {
			t.Errorf("auth/keepalive = %q/%q/%d", canonical.AuthHeader, canonical.AuthScheme, canonical.KeepAliveMs)
		}
		if canonical.DefaultModel != "voice-agent" || canonical.WsURL != "wss://agent.deepgram.com/v1/agent/converse" {
			t.Errorf("default/ws = %q/%q", canonical.DefaultModel, canonical.WsURL)
		}
	})
	t.Run("providers without a realtimeConfig return nil", func(t *testing.T) {
		for _, id := range []string{"openai", "claude", "elevenlabs", "not-a-provider"} {
			if got := GetRealtimeConfig(id); got != nil {
				t.Errorf("GetRealtimeConfig(%q) = non-nil", id)
			}
		}
	})
	t.Run("SupportsRealtimeVoice truth table", func(t *testing.T) {
		cases := []struct {
			provider string
			want     bool
		}{
			{"gemini", true}, {"deepgram", true}, {"dg", true},
			{"claude", false}, {"elevenlabs", false}, {"not-a-provider", false},
		}
		for _, tt := range cases {
			if got := SupportsRealtimeVoice(tt.provider); got != tt.want {
				t.Errorf("SupportsRealtimeVoice(%q) = %v, want %v", tt.provider, got, tt.want)
			}
		}
	})
}

func TestGetLiveCaptionConfig(t *testing.T) {
	t.Run("deepgram shape", func(t *testing.T) {
		cfg := GetLiveCaptionConfig("dg")
		if cfg == nil {
			t.Fatal("GetLiveCaptionConfig(dg) = nil")
		}
		want := &LiveCaptionConfig{WsURL: "wss://api.deepgram.com/v1/listen", AuthType: "apikey", AuthHeader: "token", Format: "deepgram"}
		if !reflect.DeepEqual(cfg, want) {
			t.Errorf("got %+v, want %+v", cfg, want)
		}
	})
	t.Run("elevenlabs shape", func(t *testing.T) {
		cfg := GetLiveCaptionConfig("el")
		if cfg == nil {
			t.Fatal("GetLiveCaptionConfig(el) = nil")
		}
		want := &LiveCaptionConfig{WsURL: "wss://api.elevenlabs.io/v1/speech-to-text/realtime", DefaultModel: "scribe_v2_realtime", AuthType: "apikey", AuthHeader: "xi-api-key", Format: "elevenlabs-scribe"}
		if !reflect.DeepEqual(cfg, want) {
			t.Errorf("got %+v, want %+v", cfg, want)
		}
	})
	t.Run("selfhosted-stt shape", func(t *testing.T) {
		cfg := GetLiveCaptionConfig("selfhosted-stt")
		if cfg == nil {
			t.Fatal("GetLiveCaptionConfig(selfhosted-stt) = nil")
		}
		want := &LiveCaptionConfig{BaseURL: "http://localhost:8080/v1/audio/transcriptions", AuthType: "apikey", AuthHeader: "bearer", Format: "openai"}
		if !reflect.DeepEqual(cfg, want) {
			t.Errorf("got %+v, want %+v", cfg, want)
		}
	})
	t.Run("gemini has no caption config", func(t *testing.T) {
		if got := GetLiveCaptionConfig("gemini"); got != nil {
			t.Errorf("GetLiveCaptionConfig(gemini) = %+v, want nil", got)
		}
	})
}

func TestSupportsLiveCaption(t *testing.T) {
	cases := []struct {
		provider string
		want     bool
	}{
		// Gate is serviceKinds liveCaption||stt — gemini passes via "stt" alone.
		{"deepgram", true}, {"dg", true},
		{"elevenlabs", true}, {"el", true},
		{"selfhosted-stt", true},
		{"gemini", true},
		{"groq", true}, {"openai", true}, {"assemblyai", true}, {"huggingface", true},
		// stt-free providers stay out (upstream serviceKinds has no stt/liveCaption there).
		{"claude", false}, {"openrouter", false}, {"minimax", false}, {"nvidia", false},
		{"not-a-provider", false},
	}
	for _, tt := range cases {
		t.Run(tt.provider, func(t *testing.T) {
			if got := SupportsLiveCaption(tt.provider); got != tt.want {
				t.Errorf("SupportsLiveCaption(%q) = %v, want %v", tt.provider, got, tt.want)
			}
		})
	}
}

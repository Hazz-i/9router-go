package realtime

import (
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// startDeepgramSession builds a repo+gateway wired to a gated fake Deepgram
// upstream, seeds one deepgram connection (api key + optional
// providerSpecificData) whose baseUrl override points at the fake, and
// returns the fake plus an already-dialled client socket (gated handshake).
func startDeepgramSession(t *testing.T, query string, psd map[string]any) (*fakeUpstream, *websocket.Conn) {
	t.Helper()
	return startDeepgramSessionCfg(t, query, psd, "wss://real-dg.invalid/converse")
}

// startDeepgramSessionCfg pins the fixture WsURL explicitly (the override test
// proves a connection baseUrl beats a bad registry URL). The model param is
// always set: without it the gateway routes to the default gemini provider.
func startDeepgramSessionCfg(t *testing.T, query string, psd map[string]any, fixtureWsURL string) (*fakeUpstream, *websocket.Conn) {
	t.Helper()
	var fake *fakeUpstream
	fake = newFakeUpstream(t, func(t *testing.T, conn *websocket.Conn) {
		// Record EVERY upstream frame until the socket closes: a fixed read
		// budget would starve once KeepAlive (50 ms in the fixture) ticks.
		for {
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			mt, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if mt == websocket.BinaryMessage {
				fake.recordRaw(append([]byte("BIN:"), data...))
				continue
			}
			fake.recordRaw(data)
		}
	})
	database, repo := newTestRepo(t)
	creds := map[string]any{"baseUrl": fake.URL}
	for k, v := range psd {
		creds[k] = v
	}
	seedConnection(t, database, "deepgram", "dg-upstream-key", creds)
	gateway := newGateway(t, repo, map[string]string{"deepgram": fixtureWsURL})
	if query != "" {
		query = "model=deepgram/voice-agent&" + query
	} else {
		query = "model=deepgram/voice-agent"
	}
	client := dialWS(t, gateway.URL, query, nil)
	return fake, client
}

func waitSettings(t *testing.T, fake *fakeUpstream) map[string]any {
	t.Helper()
	waitForFrameCount(t, fake, 1)
	s := unmarshalFrame(t, []byte(fake.frame(0)))
	if eventType(s) != "Settings" {
		t.Fatalf("first upstream frame type = %v, want Settings", s["type"])
	}
	return s
}

// TestDeepgramWire_SettingsIsFirstFrameWithTokenAuth locks: the Settings JSON
// is the first upstream frame, auth is `Authorization: Token <key>` and the
// audio block carries the exact registry shape.
func TestDeepgramWire_SettingsIsFirstFrameWithTokenAuth(t *testing.T) {
	fake, client := startDeepgramSession(t, "", nil)
	fake.open()
	settings := waitSettings(t, fake)

	if got, _ := fake.lastAuth.Load().(string); got != "Token dg-upstream-key" {
		t.Errorf("upstream Authorization header = %q, want %q", got, "Token dg-upstream-key")
	}

	audio := settings["audio"].(map[string]any)
	wantAudio := map[string]any{
		"input":  map[string]any{"encoding": "linear16", "sample_rate": float64(16000)},
		"output": map[string]any{"encoding": "linear16", "sample_rate": float64(24000), "container": "none"},
	}
	if diff := deepEqual(audio, wantAudio); diff != "" {
		t.Errorf("settings.audio = %v, want %v (%s)", audio, wantAudio, diff)
	}
	agent := settings["agent"].(map[string]any)
	if diff := deepEqual(agent["listen"].(map[string]any), map[string]any{
		"provider": map[string]any{"type": "deepgram", "model": "nova-3"},
	}); diff != "" {
		t.Errorf("agent.listen = %v, want the registry default (%s)", agent["listen"], diff)
	}
	if diff := deepEqual(agent["speak"].(map[string]any), map[string]any{
		"provider": map[string]any{"type": "deepgram", "model": "aura-2-thalia-en"},
	}); diff != "" {
		t.Errorf("agent.speak = %v, want the registry default (%s)", agent["speak"], diff)
	}
	think := agent["think"].(map[string]any)
	if diff := deepEqual(think["provider"].(map[string]any), map[string]any{
		"type": "google", "model": "gemini-3.1-flash-lite",
	}); diff != "" {
		t.Errorf("agent.think.provider = %v, want the registry default (%s)", think["provider"], diff)
	}
	if think["prompt"] != defaultInstructions {
		t.Errorf("agent.think.prompt = %v, want the session instructions", think["prompt"])
	}
	if _, hasEndpoint := think["endpoint"]; hasEndpoint {
		t.Errorf("agent.think.endpoint must be absent without providerSpecificData think credentials")
	}
	if _, hasGreeting := agent["greeting"]; hasGreeting {
		t.Errorf("agent.greeting must be absent without ?greeting=")
	}

	// Client audio rides raw binary upstream — both the JSON and binary paths.
	// Scanned rather than indexed: the 50 ms KeepAlive may interleave between
	// Settings and the audio frames.
	if err := client.WriteMessage(websocket.BinaryMessage, []byte{1, 2, 3, 4}); err != nil {
		t.Fatalf("client binary: %v", err)
	}
	writeJSON(t, client, map[string]any{"type": "input_audio_buffer.append", "audio": "AQIDBA=="})
	wantBin := "BIN:\x01\x02\x03\x04"
	waitFor(t, 3*time.Second, "two raw binary audio frames upstream", func() bool {
		n := 0
		for i := 0; i < fake.frameCount(); i++ {
			if fake.frame(i) == wantBin {
				n++
			}
		}
		return n >= 2
	})
}

// TestDeepgramWire_KeepAliveEveryKeepAliveMs: KeepAlive frames ride after
// Settings on the configured interval.
func TestDeepgramWire_KeepAliveEveryKeepAliveMs(t *testing.T) {
	fake, _ := startDeepgramSession(t, "", nil)
	fake.open()
	waitSettings(t, fake)
	waitForFrameCount(t, fake, 2) // Settings + first KeepAlive @50ms
	ka := unmarshalFrame(t, []byte(fake.frame(1)))
	if eventType(ka) != "KeepAlive" {
		t.Errorf("frame after Settings = %v, want KeepAlive", ka)
	}
}

// TestDeepgramWire_ThinkEndpointHopRidesBothCredentials: the endpoint hop is
// present ONLY when providerSpecificData carries both thinkApiKey and thinkUrl,
// and it coexists with an overridden think provider.
func TestDeepgramWire_ThinkEndpointHopRidesBothCredentials(t *testing.T) {
	psd := map[string]any{"thinkApiKey": "sk-LLM", "thinkUrl": "https://llm.example/v1/chat"}
	fake, _ := startDeepgramSession(t, "think=open_ai/gpt-4o-mini", psd)
	fake.open()
	settings := waitSettings(t, fake)

	think := settings["agent"].(map[string]any)["think"].(map[string]any)
	if diff := deepEqual(think["provider"].(map[string]any), map[string]any{
		"type": "open_ai", "model": "gpt-4o-mini",
	}); diff != "" {
		t.Errorf("agent.think.provider = %v, want the override (%s)", think["provider"], diff)
	}
	wantEndpoint := map[string]any{
		"url":     "https://llm.example/v1/chat",
		"headers": map[string]any{"Authorization": "Bearer sk-LLM"},
	}
	if diff := deepEqual(think["endpoint"].(map[string]any), wantEndpoint); diff != "" {
		t.Errorf("agent.think.endpoint = %v, want %v (%s)", think["endpoint"], wantEndpoint, diff)
	}
}

// TestDeepgramWire_UpstreamEventMappingOrderWithIDRotation locks the full
// upstream→client mapping and the response-id rotation after a cancel.
func TestDeepgramWire_UpstreamEventMappingOrderWithIDRotation(t *testing.T) {
	var fake *fakeUpstream
	fake = newFakeUpstream(t, func(t *testing.T, conn *websocket.Conn) {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		mt, data, err := conn.ReadMessage()
		if err != nil || mt != websocket.TextMessage {
			t.Errorf("expected Settings first, got err=%v mt=%d", err, mt)
			return
		}
		fake.recordRaw(data)

		writeJSON(t, conn, map[string]any{"type": "Welcome", "request_id": "req-1"})
		writeJSON(t, conn, map[string]any{"type": "SettingsApplied"})
		if err := conn.WriteMessage(websocket.BinaryMessage, []byte{1, 2, 3, 4}); err != nil {
			t.Errorf("write upstream audio: %v", err)
		}
		writeJSON(t, conn, map[string]any{"type": "ConversationText", "role": "assistant", "content": "hi there"})
		writeJSON(t, conn, map[string]any{"type": "ConversationText", "role": "user", "content": "not echoed"})
		writeJSON(t, conn, map[string]any{"type": "UserStartedSpeaking"})
		writeJSON(t, conn, map[string]any{"type": "AgentAudioDone"})
		writeJSON(t, conn, map[string]any{"type": "Error", "code": float64(400), "description": "bad request"})
		time.Sleep(300 * time.Millisecond) // let the client drain before the fake closes
	})
	database, repo := newTestRepo(t)
	// The fake must be wired through the connection's baseUrl override: the
	// fixture registry URL is deliberately unresolvable, which is exactly how
	// the previous wiring failed ("dial tcp: lookup real-dg.invalid").
	seedConnection(t, database, "deepgram", "dg-upstream-key", map[string]any{"baseUrl": fake.URL})
	gateway := newGateway(t, repo, map[string]string{"deepgram": "wss://real-dg.invalid/converse"})
	client := dialWS(t, gateway.URL, "model=deepgram/voice-agent", nil)
	fake.open()

	wantOrder := []string{
		"session.created", "session.updated",
		"response.audio.delta",
		"response.audio_transcript.delta",
		"input_audio_buffer.speech_started", "response.cancelled",
		"response.audio.done", "response.done",
		"error",
	}
	var gotOrder []string
	var firstID, secondID string
	for len(gotOrder) < len(wantOrder) {
		ev := readJSON(t, client, 3*time.Second)
		kind := eventType(ev)
		gotOrder = append(gotOrder, kind)
		switch kind {
		case "response.audio.delta":
			if id := ev["response_id"].(string); firstID == "" {
				firstID = id
			}
			if ev["output_index"].(float64) != 0 || ev["content_index"].(float64) != 0 {
				t.Errorf("audio delta indexes = %v/%v, want 0/0", ev["output_index"], ev["content_index"])
			}
			if ev["delta"] != "AQIDBA==" {
				t.Errorf("audio delta = %v, want base64 of 01 02 03 04", ev["delta"])
			}
		case "response.audio_transcript.delta":
			if ev["delta"] != "hi there" {
				t.Errorf("transcript delta = %v, want the assistant content", ev["delta"])
			}
		case "input_audio_buffer.speech_started":
			if _, has := ev["audio_start_ms"]; has {
				t.Errorf("deepgram speech_started must NOT carry audio_start_ms")
			}
		case "response.cancelled":
			if id := ev["response_id"].(string); id != firstID {
				t.Errorf("response.cancelled carried %q, want the in-flight id %q", id, firstID)
			}
		case "response.done":
			resp := ev["response"].(map[string]any)
			if secondID == "" {
				secondID, _ = resp["id"].(string)
			}
		case "error":
			errBody := ev["error"].(map[string]any)
			if errBody["type"] != "upstream_error" || errBody["message"] != "400: bad request" {
				t.Errorf("error event = %v, want upstream_error '400: bad request'", errBody)
			}
		}
	}
	if strings.Join(gotOrder, ",") != strings.Join(wantOrder, ",") {
		t.Fatalf("event order = %v, want %v", gotOrder, wantOrder)
	}
	if secondID == "" || secondID == firstID {
		t.Errorf("response ids must rotate after a cancel: first %q, second %q", firstID, secondID)
	}
}

// TestDeepgramWire_OverridesLandInSettings is the table of ?listen/?think/
// ?speak/?greeting override shapes, including the `custom:<id>` passthrough
// and the rest-of-slash id parsing.
func TestDeepgramWire_OverridesLandInSettings(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		kind     string // "listen", "think" or "speak"
		wantFrag map[string]any
	}{
		{
			name:     "listen versioned settings override",
			query:    "listen=deepgram/flux-general-multi",
			kind:     "listen",
			wantFrag: map[string]any{"provider": map[string]any{"type": "deepgram", "version": "v2", "model": "flux-general-multi"}},
		},
		{
			name:  "think override keeps prompt",
			query: "think=google/gemini-2.5-flash",
			kind:  "think",
			wantFrag: map[string]any{
				"provider": map[string]any{"type": "google", "model": "gemini-2.5-flash"},
				"prompt":   defaultInstructions,
			},
		},
		{
			name:     "speak rest-of-slash id",
			query:    "speak=open_ai/tts-1/alloy",
			kind:     "speak",
			wantFrag: map[string]any{"provider": map[string]any{"type": "open_ai", "model": "tts-1", "voice": "alloy"}},
		},
		{
			name:     "speak eleven labs premade voice",
			query:    "speak=eleven_labs/cgSgspJ2msm6clMCkdW9",
			kind:     "speak",
			wantFrag: map[string]any{"provider": map[string]any{"type": "eleven_labs", "model_id": "eleven_multilingual_v2", "voice_id": "cgSgspJ2msm6clMCkdW9"}},
		},
		{
			name:  "think slashed model preserved",
			query: "think=groq/openai/gpt-oss-20b",
			kind:  "think",
			wantFrag: map[string]any{
				"provider": map[string]any{"type": "groq", "model": "openai/gpt-oss-20b"},
				"prompt":   defaultInstructions,
			},
		},
		{
			name:     "custom speak passthrough substitutes @id",
			query:    "speak=eleven_labs/custom:my-cloned-voice",
			kind:     "speak",
			wantFrag: map[string]any{"provider": map[string]any{"type": "eleven_labs", "model_id": "eleven_multilingual_v2", "voice_id": "my-cloned-voice"}},
		},
		{
			name:  "custom think passthrough free-form id",
			query: "think=open_ai/custom:ft-gpt-5.5-myorg",
			kind:  "think",
			wantFrag: map[string]any{
				"provider": map[string]any{"type": "open_ai", "model": "ft-gpt-5.5-myorg"},
				"prompt":   defaultInstructions,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake, _ := startDeepgramSession(t, tt.query, nil)
			fake.open()
			settings := waitSettings(t, fake)
			agent := settings["agent"].(map[string]any)
			frag := agent[tt.kind].(map[string]any)
			if diff := deepEqual(frag, tt.wantFrag); diff != "" {
				t.Errorf("agent.%s = %v, want %v (%s)", tt.kind, frag, tt.wantFrag, diff)
			}
		})
	}

	t.Run("greeting maps to agent.greeting", func(t *testing.T) {
		fake, _ := startDeepgramSession(t, "greeting=Hello there!", nil)
		fake.open()
		settings := waitSettings(t, fake)
		agent := settings["agent"].(map[string]any)
		if agent["greeting"] != "Hello there!" {
			t.Errorf("agent.greeting = %v, want %q", agent["greeting"], "Hello there!")
		}
	})
}

// TestDeepgramWire_UnknownChoiceFailsBeforeDial: a bogus override surfaces as
// the realtime_config_error event + close 4400 with the exact upstream
// message, and ZERO upstream sockets are ever opened.
func TestDeepgramWire_UnknownChoiceFailsBeforeDial(t *testing.T) {
	tests := []struct {
		name  string
		query string
	}{
		{name: "unknown speak provider", query: "speak=minimax/custom:my-voice"},
		{name: "unknown think id", query: "think=google/gpt-9-turbo-nova"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake, client := startDeepgramSession(t, tt.query, nil)
			fake.open()

			ev := readJSON(t, client, 3*time.Second)
			if eventType(ev) != "error" {
				t.Fatalf("expected an error event, got %v", ev)
			}
			errBody := ev["error"].(map[string]any)
			if errBody["type"] != "invalid_request_error" || errBody["code"] != "realtime_config_error" {
				t.Errorf("error body = %v, want invalid_request_error/realtime_config_error", errBody)
			}
			msg, _ := errBody["message"].(string)
			if !strings.HasPrefix(msg, "Unknown realtime ") || !strings.Contains(msg, "Valid options:") {
				t.Errorf("error message = %q, want the Unknown realtime ... Valid options: shape", msg)
			}
			if tt.name == "unknown think id" && !strings.Contains(msg, "groq/openai/gpt-oss-20b") {
				t.Errorf("valid-options list must include slashed ids, got %q", msg)
			}

			closeCode := readClose(t, client)
			if closeCode.code != 4400 {
				t.Errorf("close code = %d, want 4400", closeCode.code)
			}
			if fake.conns.Load() != 0 {
				t.Errorf("upstream connections = %d, want 0 — validation must fail before any dial", fake.conns.Load())
			}
		})
	}
}

//go:build integration

package integration

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	json "encoding/json/v2"

	"github.com/gorilla/websocket"
)

// The two WebSocket transports of the realtime wave, end to end through the
// production router (app.ProvideRouter: RequireApiKey → RequireRateLimit →
// guardrails → handler). The fake upstream is a websocket.Upgrader inside
// httptest.NewServer, and the gateway reaches it through a providerConnection
// row whose data.baseUrl names the fake — no credential and no real network is
// involved anywhere.
//
// Client auth rides the query string (?apiKey=), which is the browser-socket
// seam in middleware.ExtractApiKey: a browser cannot set headers on a
// handshake.

// wsURL converts the env's http base into a ws URL with query parameters.
func wsURL(base, path, query string) string {
	return "ws" + strings.TrimPrefix(base, "http") + path + "?" + query
}

// dialGatewayWS dials one gateway WebSocket endpoint, failing the test on a
// handshake error.
func dialGatewayWS(t *testing.T, rawURL string) *websocket.Conn {
	t.Helper()
	dialer := &websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, resp, err := dialer.Dial(rawURL, nil)
	if err != nil {
		status := 0
		var body string
		if resp != nil {
			status = resp.StatusCode
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			body = string(raw)
		}
		t.Fatalf("dial %s: %v (status %d, body %q)", rawURL, err, status, body)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// readFrame reads one JSON text frame with a deadline.
func readFrame(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read gateway frame: %v", err)
	}
	var frame map[string]any
	if err := json.Unmarshal(msg, &frame); err != nil {
		t.Fatalf("decode gateway frame %q: %v", msg, err)
	}
	return frame
}

// waitFrame reads frames until one of the named type arrives, failing on the
// deadline or on an error frame.
func waitFrame(t *testing.T, conn *websocket.Conn, want string) map[string]any {
	t.Helper()
	for {
		frame := readFrame(t, conn)
		if frame["type"] == want {
			return frame
		}
		if frame["type"] == "error" {
			t.Fatalf("got error frame waiting for %s: %v", want, frame["error"])
		}
	}
}

// addWSConnection seeds a providerConnection row whose baseUrl names the fake
// upstream directly (Env.AddConnection appends /chat/completions, which a
// socket lane must not inherit), then reads it back. An explicit baseUrl
// override names a path under the fake; empty uses the bare fake URL.
func addWSConnection(t *testing.T, env *Env, id, provider, baseUrl string, up *Upstream) {
	t.Helper()
	if baseUrl == "" {
		baseUrl = up.URL
	}
	priority := 1
	data := fmt.Sprintf(`{"apiKey":%q,"baseUrl":%q}`, "sk-upstream", baseUrl)
	if err := env.Repo.CreateProviderConnectionFull(id, provider, "apikey", provider, &priority, data); err != nil {
		t.Fatalf("create connection %s: %v", id, err)
	}
	stored, err := env.Repo.GetProviderConnectionByID(id)
	if err != nil || stored == nil || !strings.Contains(stored.Data, up.URL) {
		t.Fatalf("connection %s did not persist the fake upstream URL %q: %v", id, up.URL, err)
	}
}

// TestRealtimeVoiceWSEndToEnd dials /v1/realtime with a query key, lets the
// fake Deepgram-converse upstream confirm the session (SettingsApplied), and
// asserts the gateway's session.created frame. The setupComplete frame rides
// along so the test holds against either adapter's confirmation event.
func TestRealtimeVoiceWSEndToEnd(t *testing.T) {
	env := newEnv(t)
	up := env.NewUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		// The first upstream frame is the gateway's Deepgram Settings frame.
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"SettingsApplied"}`))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"setupComplete":true}`))
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	addWSConnection(t, env, "conn-deepgram-rt", "deepgram", "", up)

	conn := dialGatewayWS(t, wsURL(env.BaseURL, "/v1/realtime", "model=deepgram/voice-agent&apiKey="+env.APIKey))
	frame := waitFrame(t, conn, "session.created")
	session, _ := frame["session"].(map[string]any)
	if session == nil {
		t.Fatalf("session.created carried no session object: %v", frame)
	}
	if model, _ := session["model"].(string); !strings.Contains(model, "voice-agent") {
		t.Errorf("session.model = %v, want the requested deepgram/voice-agent model", session["model"])
	}
}

// TestRealtimeVoiceWSRejectsAnonymousHandshake proves the auth gate on the
// upgrade path itself: no key anywhere (header or query) must fail the
// handshake with 401, not upgrade then close.
func TestRealtimeVoiceWSRejectsAnonymousHandshake(t *testing.T) {
	env := newEnv(t)
	conn, resp, err := (&websocket.Dialer{HandshakeTimeout: 10 * time.Second}).
		Dial(wsURL(env.BaseURL, "/v1/realtime", "model=deepgram/voice-agent"), nil)
	if err == nil {
		t.Fatal("anonymous handshake upgraded; want rejection")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("anonymous handshake status = %d, want 401 (err %v)", status, err)
	}
	_ = conn
}

// TestLiveCaptionWSEndToEnd dials /v1/audio/live-transcriptions with a query
// key, asserts the `connected` then `ready` ordering, streams one audio frame
// and receives the committed `word` the upstream reports.
//
// The fake upstream is an HTTP verbose_json transcription endpoint reached
// through the selfhosted branch: assemblyai passes the capability gate
// (serviceKinds stt) and is not one of the three socket backends, so the
// session dials the connection's own baseUrl — the only branch a fake can
// intercept offline; deepgram/elevenlabs/gemini dial catalog URLs.
func TestLiveCaptionWSEndToEnd(t *testing.T) {
	env := newEnv(t)
	up := env.NewUpstream(t, JSONResponder(http.StatusOK,
		`{"text":"halo dunia","words":[{"word":"halo","start":0,"end":0.42},{"word":"dunia","start":0.42,"end":0.9}]}`))
	addWSConnection(t, env, "conn-assemblyai-cap", "assemblyai",
		up.URL+"/v1/audio/transcriptions", up)

	conn := dialGatewayWS(t, wsURL(env.BaseURL, "/v1/audio/live-transcriptions",
		"model=assemblyai/whisper-1&language=id&encoding=linear16&sample_rate=16000&apiKey="+env.APIKey))

	// Upstream ordering: connected is always first, ready second.
	connected := readFrame(t, conn)
	if connected["type"] != "connected" {
		t.Fatalf("first gateway frame = %v, want type connected", connected["type"])
	}
	if provider, _ := connected["provider"].(string); provider != "assemblyai" {
		t.Errorf("connected.provider = %v, want assemblyai", connected["provider"])
	}
	ready := waitFrame(t, conn, "ready")
	if marker, _ := ready["upstream"].(string); marker != "selfhosted-stt" {
		t.Errorf("ready.upstream = %v, want selfhosted-stt", ready["upstream"])
	}

	// One audio chunk, then finalize to flush the selfhosted buffer upstream.
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte{1, 2, 3, 4}); err != nil {
		t.Fatalf("send audio frame: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"finalize"}`)); err != nil {
		t.Fatalf("send finalize: %v", err)
	}
	word := waitFrame(t, conn, "word")
	if w, _ := word["word"].(string); !strings.EqualFold(w, "halo") {
		t.Fatalf("word frame = %v, want the upstream's first word", word)
	}
	if isFinal, _ := word["isFinal"].(bool); !isFinal {
		t.Errorf("word frame isFinal = %v, want true for a committed word", word["isFinal"])
	}
}

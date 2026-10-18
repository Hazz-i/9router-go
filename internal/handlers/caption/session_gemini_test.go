// Session tests for the Gemini Live backend.
package caption

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// newGeminiSession wires a Gemini fake upstream and dials the client.
func newGeminiSession(t *testing.T, script func(*fakeUpstream, *websocket.Conn), query string) (*fakeUpstream, *websocket.Conn) {
	t.Helper()
	up := newFakeUpstream(t, script)
	h := newTestHandler(t)
	h.geminiURL = up.wsURL()
	addConnection(t, h.repo, "g-1", "gemini", `{"apiKey":"g-key"}`)
	conn := dialClient(t, serveHandler(t, h), query)
	return up, conn
}

func TestCaptionSessionGemini(t *testing.T) {
	t.Run("setup frame is sent on open with the transcription taps", func(t *testing.T) {
		script := func(f *fakeUpstream, conn *websocket.Conn) {
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var frame map[string]any
			if json.Unmarshal(data, &frame) == nil {
				f.store("setup", frame)
			}
			idleScript(f, conn)
		}
		up, conn := newGeminiSession(t, script, "model=gemini/gemini-3.8-live&sample_rate=8000")

		expectFrameType(t, conn, "connected")
		ready := expectFrameType(t, conn, "ready")
		if ready["upstream"] != "gemini-live" {
			t.Errorf("ready upstream = %v, want gemini-live", ready["upstream"])
		}

		setup, _ := waitForStored(t, up, "setup", 3*time.Second).(map[string]any)["setup"].(map[string]any)
		if setup == nil {
			t.Fatal("no setup frame recorded")
		}
		if setup["model"] != "models/gemini-3.8-live" {
			t.Errorf("setup model = %v, want models/gemini-3.8-live", setup["model"])
		}
		if _, has := setup["inputAudioTranscription"]; !has {
			t.Errorf("setup must enable inputAudioTranscription, got %v", setup)
		}
		if _, has := setup["outputAudioTranscription"]; !has {
			t.Errorf("setup must enable outputAudioTranscription, got %v", setup)
		}
		sys, _ := setup["systemInstruction"].(map[string]any)
		if sys == nil {
			t.Fatalf("setup systemInstruction missing")
		}
		parts, _ := sys["parts"].([]any)
		firstPart, _ := parts[0].(map[string]any)
		if firstPart["text"] != "Transcribe the spoken audio verbatim in language id." {
			t.Errorf("systemInstruction = %v", firstPart["text"])
		}
		if up.upgradeQuery().Get("key") != "g-key" {
			t.Errorf("upstream key = %q, want g-key", up.upgradeQuery().Get("key"))
		}
	})

	t.Run("legacy live model ids remap to gemini-3.8-live", func(t *testing.T) {
		script := func(f *fakeUpstream, conn *websocket.Conn) {
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var frame map[string]any
			if json.Unmarshal(data, &frame) == nil {
				f.store("setup", frame)
			}
			idleScript(f, conn)
		}
		up, conn := newGeminiSession(t, script, "model=gemini/gemini-2.0-flash-exp")

		expectFrameType(t, conn, "connected")
		expectFrameType(t, conn, "ready")
		setup, _ := waitForStored(t, up, "setup", 3*time.Second).(map[string]any)["setup"].(map[string]any)
		if setup["model"] != "models/gemini-3.8-live" {
			t.Errorf("remapped model = %v, want models/gemini-3.8-live", setup["model"])
		}
	})

	t.Run("audio forwards as 16 kHz PCM regardless of negotiated rate", func(t *testing.T) {
		script := func(f *fakeUpstream, conn *websocket.Conn) {
			// read the setup frame first
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var frame map[string]any
			if json.Unmarshal(data, &frame) == nil {
				f.store("audio", frame)
			}
			idleScript(f, conn)
		}
		up, conn := newGeminiSession(t, script, "model=gemini/gemini-3.8-live&sample_rate=24000")

		expectFrameType(t, conn, "connected")
		expectFrameType(t, conn, "ready")
		if err := conn.WriteMessage(websocket.BinaryMessage, []byte{9, 9}); err != nil {
			t.Fatalf("send audio: %v", err)
		}

		audio, _ := waitForStored(t, up, "audio", 3*time.Second).(map[string]any)
		ri, _ := audio["realtimeInput"].(map[string]any)
		if ri == nil {
			t.Fatalf("frame is not realtimeInput: %v", audio)
		}
		a, _ := ri["audio"].(map[string]any)
		if a["mimeType"] != "audio/pcm;rate=16000" {
			t.Errorf("mimeType = %v, want audio/pcm;rate=16000", a["mimeType"])
		}
		if got, _ := a["data"].(string); got != base64.StdEncoding.EncodeToString([]byte{9, 9}) {
			t.Errorf("data = %q, want the base64 of the sent bytes", got)
		}
	})

	t.Run("transcription deltas become words with flat 0.4 s clock", func(t *testing.T) {
		script := func(f *fakeUpstream, conn *websocket.Conn) {
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, _, err := conn.ReadMessage(); err != nil { // setup frame
				return
			}
			_ = sendJSON(conn, map[string]any{"serverContent": map[string]any{
				"inputTranscription": map[string]any{"text": "halo "}}})
			_ = sendJSON(conn, map[string]any{"serverContent": map[string]any{
				"outputTranscription": map[string]any{"text": "dunia"}}})
			idleScript(f, conn)
		}
		_, conn := newGeminiSession(t, script, "model=gemini/gemini-3.8-live")

		expectFrameType(t, conn, "connected")
		expectFrameType(t, conn, "ready")

		word1 := expectFrameType(t, conn, "word")
		if word1["word"] != "halo" || word1["start"] != 0.0 || word1["end"] != 0.4 {
			t.Errorf("first word = %v", word1)
		}
		if _, has := word1["confidence"]; has {
			t.Errorf("gemini word must not carry confidence, got %v", word1)
		}
		word2 := expectFrameType(t, conn, "word")
		if word2["word"] != "dunia" || word2["start"] != 0.4 || word2["end"] != 0.8 {
			t.Errorf("second word = %v", word2)
		}
	})

	t.Run("in-frame errors surface without closing the client", func(t *testing.T) {
		script := func(f *fakeUpstream, conn *websocket.Conn) {
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, _, err := conn.ReadMessage(); err != nil { // setup frame
				return
			}
			_ = sendJSON(conn, map[string]any{"error": map[string]any{"message": "boom"}})
			idleScript(f, conn)
		}
		_, conn := newGeminiSession(t, script, "model=gemini/gemini-3.8-live")

		expectFrameType(t, conn, "connected")
		expectFrameType(t, conn, "ready")
		errFrame := expectFrameType(t, conn, "error")
		if errFrame["error"] != "boom" {
			t.Errorf("error = %v, want boom", errFrame["error"])
		}
		expectTimeout(t, conn, 300*time.Millisecond) // socket stays open
	})
}

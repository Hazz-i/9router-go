// Session tests for the ElevenLabs Scribe backend.
package caption

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// newElevenLabsSession wires an ElevenLabs fake upstream and dials the client.
func newElevenLabsSession(t *testing.T, script func(*websocket.Conn), query string) (*fakeUpstream, *websocket.Conn) {
	t.Helper()
	up := newFakeUpstream(t, script)
	h := newTestHandler(t)
	h.elevenlabsURL = up.wsURL()
	addConnection(t, h.repo, "el-1", "elevenlabs", `{"apiKey":"el-key"}`)
	conn := dialClient(t, serveHandler(t, h), query)
	return up, conn
}

func TestCaptionSessionElevenLabs(t *testing.T) {
	t.Run("audio chunks are base64 wrapped and the realtime model id is hardcoded", func(t *testing.T) {
		var up *fakeUpstream
		script := func(conn *websocket.Conn) {
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var f map[string]any
			if json.Unmarshal(data, &f) == nil {
				up.store("chunk", f)
			}
			idleScript(conn)
		}
		up, conn := newElevenLabsSession(t, script, "model=el/scribe_v2&sample_rate=8000")

		expectFrameType(t, conn, "connected")
		// The audio format reflects the negotiated rate even though the model
		// id never leaves the query.
		if err := conn.WriteMessage(websocket.BinaryMessage, []byte{1, 2, 3, 4}); err != nil {
			t.Fatalf("send audio: %v", err)
		}

		chunk, _ := waitForStored(t, up, "chunk", 3*time.Second).(map[string]any)
		if chunk["message_type"] != "input_audio_chunk" {
			t.Errorf("message_type = %v, want input_audio_chunk", chunk["message_type"])
		}
		if got, _ := chunk["audio_base_64"].(string); base64.StdEncoding.EncodeToString([]byte{1, 2, 3, 4}) != got {
			t.Errorf("audio_base_64 = %q, want the base64 of the sent bytes", got)
		}

		q := up.upgradeQuery()
		if q.Get("model_id") != "scribe_v2_realtime" {
			t.Errorf("model_id = %q, want the hardcoded scribe_v2_realtime", q.Get("model_id"))
		}
		if q.Get("audio_format") != "pcm_8000" {
			t.Errorf("audio_format = %q, want pcm_8000", q.Get("audio_format"))
		}
		if q.Get("include_timestamps") != "true" {
			t.Errorf("include_timestamps = %q, want true", q.Get("include_timestamps"))
		}
		if up.upgradeHeader("xi-api-key") != "el-key" {
			t.Errorf("xi-api-key = %q, want el-key", up.upgradeHeader("xi-api-key"))
		}
	})

	t.Run("language_code follows the requested language and auto omits it", func(t *testing.T) {
		tests := []struct {
			name     string
			query    string
			wantLang string
			wantCode bool
		}{
			{name: "explicit language is sent", query: "language=en", wantLang: "en", wantCode: true},
			{name: "auto omits language_code", query: "language=auto"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				up, conn := newElevenLabsSession(t, idleScript, "model=el/scribe_v2_realtime&"+tt.query)
				expectFrameType(t, conn, "connected")
				waitForUpgrade(t, up)
				q := up.upgradeQuery()
				t.Logf("recorded query: %v", q)
				if got, has := q["language_code"]; tt.wantCode {
					if !has || got[0] != tt.wantLang {
						t.Errorf("language_code = %v, want %q", got, tt.wantLang)
					}
				} else if has {
					t.Errorf("language_code = %v, want it omitted", got)
				}
			})
		}
	})

	t.Run("session_started emits ready then plain commits synthesize timings", func(t *testing.T) {
		script := func(conn *websocket.Conn) {
			_ = sendJSON(conn, map[string]any{"message_type": "session_started", "session_id": "s1"})
			_ = sendJSON(conn, map[string]any{"message_type": "committed_transcript", "text": "halo dunia"})
			_ = sendJSON(conn, map[string]any{
				"message_type": "partial_transcript",
				"text":         "  lama  ",
			})
			idleScript(conn)
		}
		_, conn := newElevenLabsSession(t, script, "model=el/scribe_v2_realtime")

		expectFrameType(t, conn, "connected")
		ready := expectFrameType(t, conn, "ready")
		if ready["upstream"] != "elevenlabs-scribe" {
			t.Errorf("ready upstream = %v, want elevenlabs-scribe", ready["upstream"])
		}

		word1 := expectFrameType(t, conn, "word")
		if word1["word"] != "halo" || word1["start"] != 0.0 || word1["end"] != 0.3 || word1["confidence"] != 0.95 {
			t.Errorf("first word = %v", word1)
		}
		word2 := expectFrameType(t, conn, "word")
		// Faithful upstream quirk: within one committed_transcript every word
		// carries the same synthesized 0..0.3 window; the clock advances once
		// per commit.
		if word2["word"] != "dunia" || word2["start"] != 0.0 || word2["end"] != 0.3 {
			t.Errorf("second word = %v", word2)
		}
		chunkFrame := expectFrameType(t, conn, "chunk")
		chunk, _ := chunkFrame["chunk"].(map[string]any)
		if chunk["text"] != "halo dunia" || chunk["start"] != 0.0 || chunk["end"] != 0.6 {
			t.Errorf("chunk = %v, want 0..0.6 (max(0.4, 2*0.3))", chunk)
		}
		if _, has := chunk["words"]; has {
			t.Errorf("elevenlabs chunk must not carry words, got %v", chunk)
		}

		partial := expectFrameType(t, conn, "partial")
		if partial["text"] != "lama" || partial["isFinal"] != false {
			t.Errorf("partial = %v, want text=lama isFinal=false", partial)
		}
	})

	t.Run("timestamped commits carry logprob confidence", func(t *testing.T) {
		script := func(conn *websocket.Conn) {
			_ = sendJSON(conn, map[string]any{
				"message_type": "committed_transcript_with_timestamps",
				"text":         "halo dunia",
				"words": []map[string]any{
					{"type": "word", "text": " halo ", "start": 1.0, "end": 1.4, "logprob": -0.1},
					{"type": "word", "text": "dunia", "start": 1.4, "end": 1.9, "logprob": 1},
				},
			})
			idleScript(conn)
		}
		_, conn := newElevenLabsSession(t, script, "model=el/scribe_v2_realtime")

		expectFrameType(t, conn, "connected")
		word1 := expectFrameType(t, conn, "word")
		if word1["word"] != "halo" || word1["start"] != 1.0 || word1["end"] != 1.4 {
			t.Errorf("first word = %v", word1)
		}
		if c, _ := word1["confidence"].(float64); math.Abs(c-math.Exp(-0.1)) > 1e-9 {
			t.Errorf("confidence = %v, want exp(-0.1)", word1["confidence"])
		}
		word2 := expectFrameType(t, conn, "word")
		// A positive logprob clamps to 1 via Math.min(1, exp(logprob)).
		if c, _ := word2["confidence"].(float64); c != 1 {
			t.Errorf("confidence = %v, want clamped to 1", word2["confidence"])
		}
		chunkFrame := expectFrameType(t, conn, "chunk")
		chunk, _ := chunkFrame["chunk"].(map[string]any)
		if chunk["text"] != "halo dunia" || chunk["start"] != 1.0 || chunk["end"] != 1.9 {
			t.Errorf("chunk = %v, want the committed span 1.0..1.9", chunk)
		}
	})

	t.Run("finalize sends an empty commit chunk", func(t *testing.T) {
		var up *fakeUpstream
		script := func(conn *websocket.Conn) {
			_ = sendJSON(conn, map[string]any{"message_type": "session_started"})
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var f map[string]any
			if json.Unmarshal(data, &f) == nil {
				up.store("commit", f)
			}
			idleScript(conn)
		}
		up, conn := newElevenLabsSession(t, script, "model=el/scribe_v2_realtime")

		expectFrameType(t, conn, "connected")
		expectFrameType(t, conn, "ready")
		if err := conn.WriteJSON(map[string]any{"type": "finalize"}); err != nil {
			t.Fatalf("send finalize: %v", err)
		}

		commit, _ := waitForStored(t, up, "commit", 3*time.Second).(map[string]any)
		if commit["message_type"] != "input_audio_chunk" || commit["audio_base_64"] != "" || commit["commit"] != true {
			t.Errorf("finalize commit = %v", commit)
		}
	})
}

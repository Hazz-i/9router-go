// Session tests for the Deepgram backend.
package caption

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// dgWord builds one Deepgram word entry.
func dgWord(word, punctuated string, start, end float64) map[string]any {
	w := map[string]any{"word": word, "start": start, "end": end, "confidence": 0.9}
	if punctuated != "" {
		w["punctuated_word"] = punctuated
	}
	return w
}

// dgResults builds a Deepgram Results frame.
func dgResults(transcript string, isFinal bool, words ...map[string]any) map[string]any {
	return map[string]any{
		"type": "Results", "is_final": isFinal,
		"channel": map[string]any{
			"alternatives": []map[string]any{{"transcript": transcript, "words": words}},
		},
	}
}

// newDeepgramSession wires a Deepgram fake upstream, a seeded connection
// and a client dial with the given query.
func newDeepgramSession(t *testing.T, script func(*fakeUpstream, *websocket.Conn), query string) (*Handler, *fakeUpstream, *websocket.Conn) {
	t.Helper()
	up := newFakeUpstream(t, script)
	h := newTestHandler(t)
	h.deepgramURL = up.wsURL()
	addConnection(t, h.repo, "dg-1", "deepgram", `{"apiKey":"dg-key"}`)
	conn := dialClient(t, serveHandler(t, h), query)
	return h, up, conn
}

func TestCaptionSessionDeepgram(t *testing.T) {
	t.Run("connected is the first frame and ready follows upstream open", func(t *testing.T) {
		_, up, conn := newDeepgramSession(t, idleScript, "model=dg/nova-3")

		frame := expectFrameType(t, conn, "connected")
		if frame["provider"] != "deepgram" {
			t.Errorf("provider = %v, want deepgram", frame["provider"])
		}
		if frame["model"] != "nova-3" || frame["language"] != "id" || frame["encoding"] != "linear16" {
			t.Errorf("connected echo = %v", frame)
		}
		if sr, _ := frame["sampleRate"].(float64); sr != 16000 {
			t.Errorf("sampleRate = %v, want 16000", frame["sampleRate"])
		}
		ready := expectFrameType(t, conn, "ready")
		if ready["upstream"] != "deepgram" {
			t.Errorf("ready upstream = %v, want deepgram", ready["upstream"])
		}

		q := up.upgradeQuery()
		for k, want := range map[string]string{
			"model": "nova-3", "language": "id", "smart_format": "true",
			"interim_results": "true", "punctuate": "true", "endpointing": "300",
		} {
			if got := q.Get(k); got != want {
				t.Errorf("upstream param %s = %q, want %q", k, got, want)
			}
		}
		if up.upgradeHeader("Authorization") != "Token dg-key" {
			t.Errorf("Authorization = %q, want Token dg-key", up.upgradeHeader("Authorization"))
		}
	})

	t.Run("encoding params follow the requested encoding", func(t *testing.T) {
		tests := []struct {
			name       string
			query      string
			wantKeys   map[string]string
			rejectKeys []string
		}{
			{
				name:     "linear16 sends encoding and sample rate",
				query:    "model=dg/nova-3&encoding=linear16&sample_rate=16000",
				wantKeys: map[string]string{"encoding": "linear16", "sample_rate": "16000"},
			},
			{
				name:     "pcm is treated as linear16",
				query:    "model=dg/nova-3&encoding=pcm",
				wantKeys: map[string]string{"encoding": "linear16", "sample_rate": "16000"},
			},
			{
				name:       "anything else lets Deepgram auto-detect",
				query:      "model=dg/nova-3&encoding=webm_opus",
				rejectKeys: []string{"encoding", "sample_rate"},
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				_, up, conn := newDeepgramSession(t, idleScript, tt.query)
				expectFrameType(t, conn, "connected")
				expectFrameType(t, conn, "ready")
				q := up.upgradeQuery()
				for k, want := range tt.wantKeys {
					if got := q.Get(k); got != want {
						t.Errorf("param %s = %q, want %q", k, got, want)
					}
				}
				for _, k := range tt.rejectKeys {
					if _, ok := q[k]; ok {
						t.Errorf("param %s must be absent, got %q", k, q.Get(k))
					}
				}
			})
		}
	})

	t.Run("interims ride partial and finals commit words plus a chunk", func(t *testing.T) {
		script := func(f *fakeUpstream, conn *websocket.Conn) {
			_ = sendJSON(conn, dgResults("halo dunia", false,
				dgWord("halo", "", 0, 0.4), dgWord("dunia", "", 0.44, 0.9)))
			_ = sendJSON(conn, dgResults("halo dunia.", true,
				dgWord("halo", "Halo", 0, 0.4), dgWord("dunia", "dunia.", 0.44, 0.9)))
			idleScript(f, conn)
		}
		_, _, conn := newDeepgramSession(t, script, "model=dg/nova-3")

		expectFrameType(t, conn, "connected")
		expectFrameType(t, conn, "ready")

		partial := expectFrameType(t, conn, "partial")
		if partial["text"] != "halo dunia" {
			t.Errorf("partial text = %v", partial["text"])
		}
		if _, has := partial["isFinal"]; has {
			t.Errorf("deepgram partial must not carry isFinal, got %v", partial)
		}

		first := expectFrameType(t, conn, "word")
		if first["word"] != "Halo" {
			t.Errorf("deepgram must prefer punctuated_word, got %v", first["word"])
		}
		if s, _ := first["start"].(float64); s != 0 {
			t.Errorf("word start = %v, want 0", first["start"])
		}
		second := expectFrameType(t, conn, "word")
		if second["word"] != "dunia." || second["end"] != 0.9 {
			t.Errorf("second word = %v", second)
		}
		if second["isFinal"] != true {
			t.Errorf("word isFinal = %v, want true", second["isFinal"])
		}

		chunkFrame := expectFrameType(t, conn, "chunk")
		chunk, _ := chunkFrame["chunk"].(map[string]any)
		if chunk["text"] != "Halo dunia." || chunk["start"] != 0.0 || chunk["end"] != 0.9 {
			t.Errorf("chunk = %v", chunk)
		}
		if _, has := chunk["words"]; !has {
			t.Errorf("deepgram chunk must carry words, got %v", chunk)
		}
	})

	t.Run("finalize dedups the re-emitted committed segment", func(t *testing.T) {
		script := func(f *fakeUpstream, conn *websocket.Conn) {
			_ = sendJSON(conn, dgResults("halo dunia.", true,
				dgWord("halo", "Halo", 0, 0.4), dgWord("dunia", "dunia.", 0.44, 0.9)))
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			for {
				_, data, err := conn.ReadMessage()
				if err != nil {
					return
				}
				var f map[string]any
				if json.Unmarshal(data, &f) != nil {
					continue
				}
				if f["type"] == "Finalize" {
					break
				}
			}
			// A Finalize makes Deepgram re-emit the committed segment, then a
			// genuinely new word arrives.
			_ = sendJSON(conn, dgResults("halo dunia.", true,
				dgWord("halo", "Halo", 0, 0.4), dgWord("dunia", "dunia.", 0.44, 0.9)))
			_ = sendJSON(conn, dgResults("satu.", true, dgWord("satu", "satu.", 1.2, 1.6)))
			idleScript(f, conn)
		}
		_, _, conn := newDeepgramSession(t, script, "model=dg/nova-3")

		expectFrameType(t, conn, "connected")
		expectFrameType(t, conn, "ready")
		expectFrameType(t, conn, "word")
		expectFrameType(t, conn, "word")
		expectFrameType(t, conn, "chunk")

		if err := conn.WriteJSON(map[string]any{"type": "finalize"}); err != nil {
			t.Fatalf("send finalize: %v", err)
		}

		// The re-emit commits nothing: the next frame is the re-computed
		// chunk, not duplicate words.
		afterFinalize := expectFrameType(t, conn, "chunk")
		chunk, _ := afterFinalize["chunk"].(map[string]any)
		if chunk["text"] != "Halo dunia." {
			t.Errorf("re-emit chunk = %v, want the committed span without duplicates", chunk)
		}
		next := expectFrameType(t, conn, "word")
		if next["word"] != "satu." {
			t.Errorf("first new word after re-emit = %v, want satu.", next["word"])
		}
	})

	t.Run("keepalive forwards as KeepAlive and audio passes through raw", func(t *testing.T) {
		script := func(f *fakeUpstream, conn *websocket.Conn) {
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var frame map[string]any
			if json.Unmarshal(data, &frame) == nil {
				f.store("keepalive", frame)
			}
			mt, audio, err := conn.ReadMessage()
			if err != nil {
				return
			}
			// record (never assert) from the server goroutine; the test
			// goroutine checks the frame type below.
			f.store("binaryType", mt)
			f.store("binary", audio)
			idleScript(f, conn)
		}
		_, up, conn := newDeepgramSession(t, script, "model=dg/nova-3")

		expectFrameType(t, conn, "connected")
		expectFrameType(t, conn, "ready")
		if err := conn.WriteJSON(map[string]any{"type": "keepalive"}); err != nil {
			t.Fatalf("send keepalive: %v", err)
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, []byte{1, 2, 3}); err != nil {
			t.Fatalf("send audio: %v", err)
		}

		echoed, _ := waitForStored(t, up, "keepalive", 3*time.Second).(map[string]any)
		if echoed["type"] != "KeepAlive" {
			t.Errorf("keepalive forwarded as %v, want KeepAlive", echoed)
		}
		audio, _ := waitForStored(t, up, "binary", 3*time.Second).([]byte)
		if len(audio) != 3 || audio[0] != 1 {
			t.Errorf("audio forwarded as %v, want the raw bytes", audio)
		}
		if mt, _ := waitForStored(t, up, "binaryType", 3*time.Second).(int); mt != websocket.BinaryMessage {
			t.Errorf("upstream frame type = %d, want binary", mt)
		}
	})

	t.Run("upstream closed leaves the client socket open", func(t *testing.T) {
		script := func(f *fakeUpstream, conn *websocket.Conn) {
			_ = sendJSON(conn, dgResults("halo dunia.", true,
				dgWord("halo", "Halo", 0, 0.4), dgWord("dunia", "dunia.", 0.44, 0.9)))
			// returning closes the upstream socket
		}
		_, _, conn := newDeepgramSession(t, script, "model=dg/nova-3")

		expectFrameType(t, conn, "connected")
		expectFrameType(t, conn, "ready")
		expectFrameType(t, conn, "word")
		expectFrameType(t, conn, "word")
		expectFrameType(t, conn, "chunk")
		closed := expectFrameType(t, conn, "upstream_closed")
		if closed["code"] == nil {
			t.Errorf("upstream_closed must carry a code, got %v", closed)
		}
		expectTimeout(t, conn, 300*time.Millisecond) // client socket still open
	})

	t.Run("usage records one prompt token per committed word exactly once", func(t *testing.T) {
		script := func(f *fakeUpstream, conn *websocket.Conn) {
			_ = sendJSON(conn, dgResults("halo dunia.", true,
				dgWord("halo", "Halo", 0, 0.4), dgWord("dunia", "dunia.", 0.44, 0.9)))
			idleScript(f, conn)
		}
		h, _, conn := newDeepgramSession(t, script, "model=dg/nova-3")

		expectFrameType(t, conn, "connected")
		expectFrameType(t, conn, "ready")
		expectFrameType(t, conn, "word")
		expectFrameType(t, conn, "word")
		_ = conn.Close()

		row := usageCount(t, h.repo, 1)
		if row.Prompt != 2 || row.Completion != 0 {
			t.Errorf("usage row = %v, want prompt=2 completion=0", row)
		}
	})
}

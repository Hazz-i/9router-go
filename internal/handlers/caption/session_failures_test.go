// Failure-shape tests (invalid model, unsupported provider, no credentials)
// and the get_subtitles round trip.
package caption

import (
	"testing"

	"github.com/gorilla/websocket"
)

func TestCaptionFailureShapes(t *testing.T) {
	t.Run("a model without a provider part closes 4400", func(t *testing.T) {
		h := newTestHandler(t)
		conn := dialClient(t, serveHandler(t, h), "model=nova-3")
		errFrame := expectFrameType(t, conn, "error")
		if errFrame["error"] != "Invalid model specification: nova-3" {
			t.Errorf("error = %v", errFrame["error"])
		}
		expectCloseError(t, conn, 4400)
	})

	t.Run("an unsupported provider closes 4400 naming the param spelling", func(t *testing.T) {
		h := newTestHandler(t)
		h.supported = func(string) bool { return false }
		conn := dialClient(t, serveHandler(t, h), "model=dg/nova-3")
		errFrame := expectFrameType(t, conn, "error")
		if errFrame["error"] != "Provider dg does not support live caption / STT" {
			t.Errorf("error = %v", errFrame["error"])
		}
		expectCloseError(t, conn, 4400)
	})

	t.Run("a provider without credentials closes 4503", func(t *testing.T) {
		up := newFakeUpstream(t, idleScript)
		h := newTestHandler(t)
		h.deepgramURL = up.wsURL()
		conn := dialClient(t, serveHandler(t, h), "model=dg/nova-3")
		errFrame := expectFrameType(t, conn, "error")
		if errFrame["error"] != "No credentials configured for provider: deepgram" {
			t.Errorf("error = %v", errFrame["error"])
		}
		expectCloseError(t, conn, 4503)
	})
}

func TestCaptionGetSubtitles(t *testing.T) {
	t.Run("get_subtitles round-trips the committed words", func(t *testing.T) {
		script := func(f *fakeUpstream, conn *websocket.Conn) {
			_ = sendJSON(conn, dgResults("halo dunia satu", true,
				dgWord("halo", "Halo", 0, 0.5),
				dgWord("dunia", "dunia", 0.55, 1.0),
				dgWord("satu", "satu", 1.05, 1.5)))
			idleScript(f, conn)
		}
		_, _, conn := newDeepgramSession(t, script, "model=dg/nova-3")

		expectFrameType(t, conn, "connected")
		expectFrameType(t, conn, "ready")
		expectFrameType(t, conn, "word")
		expectFrameType(t, conn, "word")
		expectFrameType(t, conn, "word")
		expectFrameType(t, conn, "chunk")

		if err := conn.WriteJSON(map[string]any{"type": "get_subtitles", "format": "srt"}); err != nil {
			t.Fatalf("send get_subtitles: %v", err)
		}
		sub := expectFrameType(t, conn, "subtitles")
		if sub["format"] != "srt" {
			t.Errorf("format = %v, want srt", sub["format"])
		}
		if sub["srt"] != "1\n00:00:00,000 --> 00:00:01,500\nHalo dunia satu\n" {
			t.Errorf("srt = %q", sub["srt"])
		}
		ass, _ := sub["ass"].(string)
		wantTail := "Dialogue: 0,0:00:00.00,0:00:01.50,Default,,0,0,0,,{\\k50}Halo {\\k45}dunia {\\k45}satu\n"
		if len(ass) < len(wantTail) || ass[len(ass)-len(wantTail):] != wantTail {
			t.Errorf("ass = %q, want it to end with %q", ass, wantTail)
		}
		if _, has := sub["chunks"]; !has {
			t.Errorf("subtitles frame must carry chunks")
		}
	})

	t.Run("get_subtitles with no format defaults to all", func(t *testing.T) {
		script := func(f *fakeUpstream, conn *websocket.Conn) {
			_ = sendJSON(conn, dgResults("halo dunia", true,
				dgWord("halo", "Halo", 0, 0.5), dgWord("dunia", "dunia", 0.55, 1.0)))
			idleScript(f, conn)
		}
		_, _, conn := newDeepgramSession(t, script, "model=dg/nova-3")

		expectFrameType(t, conn, "connected")
		expectFrameType(t, conn, "ready")
		expectFrameType(t, conn, "word")
		expectFrameType(t, conn, "word")
		expectFrameType(t, conn, "chunk")

		if err := conn.WriteJSON(map[string]any{"type": "get_subtitles"}); err != nil {
			t.Fatalf("send get_subtitles: %v", err)
		}
		sub := expectFrameType(t, conn, "subtitles")
		if sub["format"] != "all" {
			t.Errorf("format = %v, want all", sub["format"])
		}
		if sub["srt"] != "1\n00:00:00,000 --> 00:00:01,000\nHalo dunia\n" {
			t.Errorf("srt = %q", sub["srt"])
		}
	})
}

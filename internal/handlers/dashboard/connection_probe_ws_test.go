package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// wsProbeServer is a fake gateway WebSocket endpoint: it upgrades the dialer
// itself, records the request URL, then runs script(conn).
func wsProbeServer(t *testing.T, record **string, script func(conn *websocket.Conn)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if record != nil {
			*record = &r.URL.RawQuery
		}
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		script(conn)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// sendJSON writes one text frame and tolerates a closed client.
func sendJSON(t *testing.T, conn *websocket.Conn, frame string) {
	t.Helper()
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := conn.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
		t.Logf("fake ws write: %v", err)
	}
}

// parseQuery decodes a recorded RawQuery into single-valued keys.
func parseQuery(raw string) (map[string]string, error) {
	values, err := url.ParseQuery(raw)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(values))
	for k, v := range values {
		out[k] = v[0]
	}
	return out, nil
}

// TestProbeWebSocketModel_LiveCaptionReady pins the liveCaption probe: the URL
// is exactly the shape ping.js:218-230 builds, and the first `ready` frame
// passes regardless of which upstream marker rides it.
func TestProbeWebSocketModel_LiveCaptionReady(t *testing.T) {
	var query *string
	srv := wsProbeServer(t, &query, func(conn *websocket.Conn) {
		sendJSON(t, conn, `{"type":"connected","provider":"deepgram","model":"nova-3","language":"id"}`)
		sendJSON(t, conn, `{"type":"ready","upstream":"deepgram"}`)
	})

	out := ProbeWebSocketModel(context.Background(), srv.URL, "dg/nova-3", "liveCaption", "sk-gateway")
	if !out.OK {
		t.Fatalf("expected ready-frame success, got error %q", out.Error)
	}
	if out.Error != "" {
		t.Errorf("unexpected error field: %q", out.Error)
	}
	if out.LatencyMs < 0 {
		t.Errorf("latencyMs = %d, want >= 0", out.LatencyMs)
	}

	got, err := parseQuery(*query)
	if err != nil {
		t.Fatalf("decode recorded query %q: %v", *query, err)
	}
	for key, want := range map[string]string{
		"model":       "dg/nova-3",
		"language":    "id",
		"encoding":    "linear16",
		"sample_rate": "16000",
		"apiKey":      "sk-gateway",
	} {
		if got[key] != want {
			t.Errorf("query %s = %q, want %q", key, got[key], want)
		}
	}
}

// TestProbeWebSocketModel_RealtimeSessionCreated pins the realtime probe:
// success on `session.created`, with model and the Puck voice in the query.
func TestProbeWebSocketModel_RealtimeSessionCreated(t *testing.T) {
	var query *string
	srv := wsProbeServer(t, &query, func(conn *websocket.Conn) {
		sendJSON(t, conn, `{"type":"session.created","session":{"model":"voice-agent"}}`)
	})

	out := ProbeWebSocketModel(context.Background(), srv.URL, "deepgram/voice-agent", "realtime", "sk-gateway")
	if !out.OK {
		t.Fatalf("expected session.created success, got error %q", out.Error)
	}
	got, err := parseQuery(*query)
	if err != nil {
		t.Fatalf("decode recorded query %q: %v", *query, err)
	}
	if got["model"] != "deepgram/voice-agent" || got["voice"] != "Puck" || got["apiKey"] != "sk-gateway" {
		t.Errorf("query = %v, want model/voice=Puck/apiKey", got)
	}
}

// TestProbeWebSocketModel_ErrorFrameFails pins the error frame: an `error`
// before the success frame fails with the frame's own message.
func TestProbeWebSocketModel_ErrorFrameFails(t *testing.T) {
	srv := wsProbeServer(t, nil, func(conn *websocket.Conn) {
		sendJSON(t, conn, `{"type":"error","error":{"message":"Unauthorized: Invalid or missing API key"}}`)
		time.Sleep(50 * time.Millisecond)
	})

	out := ProbeWebSocketModel(context.Background(), srv.URL, "dg/nova-3", "liveCaption", "bad-key")
	if out.OK {
		t.Fatal("expected the error frame to fail the probe")
	}
	if !strings.Contains(out.Error, "Unauthorized: Invalid or missing API key") {
		t.Errorf("error = %q, want the frame's message", out.Error)
	}
}

// TestProbeWebSocketModel_ClosedBeforeReady pins the close path: a socket the
// gateway closes before the success frame fails with the close code.
func TestProbeWebSocketModel_ClosedBeforeReady(t *testing.T) {
	srv := wsProbeServer(t, nil, func(conn *websocket.Conn) {
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(4401, "Unauthorized"),
			time.Now().Add(5*time.Second))
		time.Sleep(50 * time.Millisecond)
	})

	out := ProbeWebSocketModel(context.Background(), srv.URL, "dg/nova-3", "liveCaption", "bad-key")
	if out.OK {
		t.Fatal("expected a close before `ready` to fail the probe")
	}
	if !strings.Contains(out.Error, "code 4401") || !strings.Contains(out.Error, "before ready") {
		t.Errorf("error = %q, want a `code 4401 ... before ready` message", out.Error)
	}
}

// TestProbeWebSocketModel_UnsupportedKind pins the guard: only the two WS kinds
// probe through a socket.
func TestProbeWebSocketModel_UnsupportedKind(t *testing.T) {
	out := ProbeWebSocketModel(context.Background(), "http://127.0.0.1:1", "openai/gpt-4", "stt", "")
	if out.OK || out.Error == "" {
		t.Fatalf("expected an unsupported-kind error, got %+v", out)
	}
}

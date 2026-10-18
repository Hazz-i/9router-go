package media

import (
	json "encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"9router/proxy/internal/db"

	"github.com/gorilla/websocket"
)

// wsProbeRequest is the upgrade request a fake WebSocket gateway recorded.
type wsProbeRequest struct {
	mu    sync.Mutex
	path  string
	query url.Values
}

func (g *wsProbeRequest) record(r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.path = r.URL.Path
	g.query = r.URL.Query()
}

func (g *wsProbeRequest) snapshot() (string, url.Values) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.path, g.query
}

// wsProbeUpstream fakes one of the gateway's WebSocket lanes: it upgrades,
// records the handshake request, writes the scripted frames in order and then
// holds the socket until the probe closes it. Write errors after the probe has
// read its verdict and gone are the normal end of the exchange, not failures.
type wsProbeUpstream struct {
	*httptest.Server
	handshake *wsProbeRequest
}

func newWSProbeUpstream(t *testing.T, frames ...string) *wsProbeUpstream {
	t.Helper()
	up := &wsProbeUpstream{handshake: &wsProbeRequest{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		up.handshake.record(r)
		for _, frame := range frames {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
				return
			}
		}
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	up.Server = srv
	return up
}

// runTestModel posts one dashboard Test request at HandleTestModel with req.Host
// pointed at the fake gateway, and decodes the chip verdict.
func runTestModel(t *testing.T, h *MediaHandler, body, host string, headers map[string]string) (*httptest.ResponseRecorder, testModelResult) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/models/test", strings.NewReader(body))
	req.Host = host
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.HandleTestModel(rec, req)

	var res testModelResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode result %q: %v", rec.Body.String(), err)
	}
	return rec, res
}

func TestHandleTestModel_WebSocketKinds(t *testing.T) {
	// The WS lanes never touch the repository: no DB, no provider credentials.
	h := NewMediaHandler(nil, nil, nil)
	auth := map[string]string{"Authorization": "Bearer test-api-key"}

	tests := []struct {
		name      string
		body      string
		frames    []string
		wantPath  string
		wantQuery map[string]string
		wantOK    bool
		wantError string
	}{
		{
			name:     "realtime succeeds on session.created",
			body:     `{"model":"openai/gpt-realtime","kind":"realtime"}`,
			frames:   []string{`{"type":"session.created"}`},
			wantPath: "/v1/realtime",
			wantQuery: map[string]string{
				"model":  "openai/gpt-realtime",
				"voice":  "Puck",
				"apiKey": "test-api-key",
			},
			wantOK: true,
		},
		{
			name:     "liveCaption succeeds on ready",
			body:     `{"model":"deepseek/ds-live","kind":"liveCaption"}`,
			frames:   []string{`{"type":"ready"}`},
			wantPath: "/v1/audio/live-transcriptions",
			wantQuery: map[string]string{
				"model":       "deepseek/ds-live",
				"language":    "id",
				"encoding":    "linear16",
				"sample_rate": "16000",
				"apiKey":      "test-api-key",
			},
			wantOK: true,
		},
		{
			name:      "error frame surfaces its message",
			body:      `{"model":"openai/gpt-realtime","kind":"realtime"}`,
			frames:    []string{`{"type":"error","error":{"message":"boom"}}`},
			wantPath:  "/v1/realtime",
			wantOK:    false,
			wantError: "boom",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			up := newWSProbeUpstream(t, tt.frames...)
			rec, res := runTestModel(t, h, tt.body, strings.TrimPrefix(up.URL, "http://"), auth)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
			}
			if res.OK != tt.wantOK {
				t.Errorf("OK = %v, want %v (error %q)", res.OK, tt.wantOK, res.Error)
			}
			if res.Error != tt.wantError {
				t.Errorf("Error = %q, want %q", res.Error, tt.wantError)
			}
			if res.LatencyMs < 0 {
				t.Errorf("LatencyMs = %d, want >= 0", res.LatencyMs)
			}

			path, query := up.handshake.snapshot()
			if path != tt.wantPath {
				t.Errorf("gateway path = %q, want %q", path, tt.wantPath)
			}
			for k, want := range tt.wantQuery {
				if got := query.Get(k); got != want {
					t.Errorf("query %s = %q, want %q", k, got, want)
				}
			}
		})
	}
}

func TestHandleTestModel_UnsupportedKindStill400(t *testing.T) {
	h := NewMediaHandler(nil, nil, nil)
	rec, res := runTestModel(t, h, `{"model":"openai/gpt-realtime","kind":"telepathy"}`, "example.com", nil)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if res.Error != "unsupported model kind: telepathy" {
		t.Errorf("Error = %q, want %q", res.Error, "unsupported model kind: telepathy")
	}
}

// TestHandleTestModel_HTTPKindsUnchanged proves the WebSocket additions did not
// disturb the recorder dispatch the other kinds run through: an embedding probe
// and a plain chat probe both still reach a seeded fake provider upstream and
// read the verdict out of its payload.
func TestHandleTestModel_HTTPKindsUnchanged(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/embeddings") {
			w.Write([]byte(`{"object":"list","data":[{"embedding":[0.1,0.2]}]}`))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"hi"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()

	database, cleanup := setupEmbeddingsTestDB(t)
	defer cleanup()
	data, _ := json.Marshal(map[string]any{
		"apiKey":  "sk-test",
		"baseUrl": upstream.URL,
	})
	if _, err := database.Exec(`INSERT INTO providerConnections (id, provider, authType, name, priority, isActive, data, createdAt, updatedAt) VALUES
		('conn-1', 'openai', 'apikey', 'OpenAI Test', 1, 1, ?, '2026-07-18T00:00:00Z', '2026-07-18T00:00:00Z')`, string(data)); err != nil {
		t.Fatalf("seed provider connection: %v", err)
	}
	h := newTestMediaHandler(db.NewRepo(database))

	tests := []struct {
		name string
		body string
	}{
		{name: "embedding kind still probes /embeddings", body: `{"model":"openai/text-embedding-ada-002","kind":"embedding"}`},
		{name: "default chat kind still probes /chat/completions", body: `{"model":"openai/gpt-4o-mini"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, res := runTestModel(t, h, tt.body, "example.com", nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
			}
			if !res.OK {
				t.Errorf("OK = false, want true (error %q)", res.Error)
			}
		})
	}
}

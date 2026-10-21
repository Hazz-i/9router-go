package realtime

import (
	"database/sql"
	json "encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"9router/proxy/internal/db"
	"9router/proxy/internal/providers"
	"github.com/gorilla/websocket"
)

// --- test fixtures -----------------------------------------------------------

const testAPIKey = "vr-test-client-key"

// fixtureConfig builds a small, behaviour-equivalent copy of the registry
// realtimeConfig shapes (the same group/choice fragment shapes Lane A ports).
func fixtureConfig(provider, wsURL string) *providers.RealtimeConfig {
	switch provider {
	case "gemini":
		return &providers.RealtimeConfig{
			DefaultModel:    "gemini-3.8-live",
			WsURL:           wsURL,
			AuthType:        "apikey",
			AuthQueryParam:  "key",
			AudioInRate:     16000,
			AudioOutRate:    24000,
			SupportedVoices: []string{"Puck", "Charon", "Kore", "Fenrir", "Aoede"},
		}
	case "deepgram":
		return &providers.RealtimeConfig{
			DefaultModel: "voice-agent",
			WsURL:        wsURL,
			AuthType:     "apikey",
			AuthHeader:   "Authorization",
			AuthScheme:   "Token",
			AudioInRate:  16000,
			AudioOutRate: 24000,
			KeepAliveMs:  50,
			Listen: &providers.RealtimeChoiceSet{
				Provider: map[string]any{"type": "deepgram", "model": "nova-3"},
				Providers: []providers.RealtimeChoiceGroup{{
					Type: "deepgram",
					Choices: []providers.RealtimeChoice{
						{ID: "nova-3"}, {ID: "nova-2"},
						{ID: "flux-general-multi", Settings: map[string]any{
							"provider": map[string]any{"type": "deepgram", "version": "v2", "model": "flux-general-multi"},
						}},
					},
				}},
			},
			Speak: &providers.RealtimeChoiceSet{
				Provider: map[string]any{"type": "deepgram", "model": "aura-2-thalia-en"},
				Providers: []providers.RealtimeChoiceGroup{
					{
						Type:        "deepgram",
						Choices:     []providers.RealtimeChoice{{ID: "aura-2-thalia-en"}},
						CustomVoice: map[string]any{"provider": map[string]any{"type": "deepgram", "model": "@id"}},
					},
					{
						Type: "eleven_labs",
						Choices: []providers.RealtimeChoice{{
							ID: "cgSgspJ2msm6clMCkdW9",
							Settings: map[string]any{
								"provider": map[string]any{"type": "eleven_labs", "model_id": "eleven_multilingual_v2", "voice_id": "cgSgspJ2msm6clMCkdW9"},
							},
						}},
						CustomVoice: map[string]any{
							"provider": map[string]any{"type": "eleven_labs", "model_id": "eleven_multilingual_v2", "voice_id": "@id"},
						},
					},
					{
						Type: "open_ai",
						Choices: []providers.RealtimeChoice{{
							ID: "tts-1/alloy",
							Settings: map[string]any{
								"provider": map[string]any{"type": "open_ai", "model": "tts-1", "voice": "alloy"},
							},
						}},
						CustomVoice: map[string]any{
							"provider": map[string]any{"type": "open_ai", "model": "tts-1", "voice": "@id"},
						},
					},
				},
			},
			Think: &providers.RealtimeChoiceSet{
				Provider: map[string]any{"type": "google", "model": "gemini-3.1-flash-lite"},
				Providers: []providers.RealtimeChoiceGroup{
					{Type: "open_ai", Choices: []providers.RealtimeChoice{{ID: "gpt-4o-mini"}}},
					{Type: "google", Choices: []providers.RealtimeChoice{{ID: "gemini-2.5-flash"}}},
					{Type: "groq", Choices: []providers.RealtimeChoice{{ID: "openai/gpt-oss-20b"}}},
				},
			},
		}
	default:
		return nil
	}
}

// --- fakes -------------------------------------------------------------------

// fakeUpstream is the stand-in provider socket: it answers the upgrade, counts
// connections, records the request headers and drives a scripted upstream
// side. The handshake is gated on open() so a test can stage client frames
// (or assert a no-dial guarantee) before the upstream side proceeds. Frames
// the script reads are recorded thread-safely for polling assertions.
type fakeUpstream struct {
	URL      string
	conns    atomic.Int32
	lastAuth atomic.Value // string

	release  chan struct{}
	released sync.Once

	frameMu sync.Mutex
	frames  []string
}

func newFakeUpstream(t *testing.T, script func(t *testing.T, conn *websocket.Conn)) *fakeUpstream {
	t.Helper()
	s := &fakeUpstream{release: make(chan struct{})}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-s.release
		s.lastAuth.Store(r.Header.Get("Authorization"))
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		s.conns.Add(1)
		defer conn.Close()
		if script != nil {
			script(t, conn)
		}
	}))
	t.Cleanup(func() {
		s.released.Do(func() { close(s.release) })
		srv.Close()
	})
	s.URL = srv.URL
	return s
}

// open releases the fake's handshake gate.
func (s *fakeUpstream) open() { s.released.Do(func() { close(s.release) }) }

// recordRaw appends one raw upstream frame to the recording.
func (s *fakeUpstream) recordRaw(data []byte) {
	s.frameMu.Lock()
	s.frames = append(s.frames, string(data))
	s.frameMu.Unlock()
}

func (s *fakeUpstream) frame(i int) string {
	s.frameMu.Lock()
	defer s.frameMu.Unlock()
	if i < len(s.frames) {
		return s.frames[i]
	}
	return ""
}

func (s *fakeUpstream) frameCount() int {
	s.frameMu.Lock()
	defer s.frameMu.Unlock()
	return len(s.frames)
}

// --- db / gateway helpers ----------------------------------------------------

func newTestRepo(t *testing.T) (*sql.DB, *db.Repo) {
	t.Helper()
	tmp, err := os.CreateTemp(t.TempDir(), "realtime_*.sqlite")
	if err != nil {
		t.Fatalf("temp db: %v", err)
	}
	// The sqlite driver opens its own handle; the CreateTemp handle must be
	// closed here or it lingers (Windows: TempDir RemoveAll fails with
	// "used by another process", nondeterministically per GC).
	_ = tmp.Close()
	database, err := db.OpenDatabase(tmp.Name())
	if err != nil {
		t.Fatalf("OpenDatabase: %v", err)
	}
	if err := db.EnsureCoreSchema(database); err != nil {
		t.Fatalf("EnsureCoreSchema: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database, db.NewRepo(database)
}

// newGateway mounts the realtime handler over the given per-provider upstream
// URLs and returns the base URL clients dial. Its cleanup drains the handler's
// sessions BEFORE the db handle closes (registered later ⇒ runs earlier than
// newTestRepo's db.Close, but after dialWS closes the client socket, which is
// what ends the last session).
func newGateway(t *testing.T, repo *db.Repo, wsURLs map[string]string) *httptest.Server {
	t.Helper()
	h := NewHandler(repo, nil)
	h.configFor = func(provider string) *providers.RealtimeConfig {
		return fixtureConfig(provider, wsURLs[provider])
	}
	srv := httptest.NewServer(http.HandlerFunc(h.HandleWS))
	t.Cleanup(srv.Close)
	t.Cleanup(h.waitSessions)
	return srv
}

// seedConnection inserts one active provider connection whose data carries the
// api key and (optionally) extra providerSpecificData fields.
func seedConnection(t *testing.T, database *sql.DB, provider, apiKey string, psd map[string]any) {
	t.Helper()
	data := map[string]any{"apiKey": apiKey}
	if psd != nil {
		data["providerSpecificData"] = psd
	}
	blob, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal conn data: %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO providerConnections (id, provider, authType, name, priority, isActive, data, createdAt, updatedAt) VALUES
		 ('conn-rt', ?, 'apikey', 'Realtime Test', 1, 1, ?, '2026-07-18T00:00:00Z', '2026-07-18T00:00:00Z')`,
		provider, string(blob)); err != nil {
		t.Fatalf("seed connection: %v", err)
	}
}

// --- frame / polling helpers -------------------------------------------------

func writeJSON(t *testing.T, conn *websocket.Conn, frame map[string]any) {
	t.Helper()
	if err := conn.WriteJSON(frame); err != nil {
		t.Errorf("write frame: %v", err)
	}
}

func readRaw(t *testing.T, conn *websocket.Conn, timeout time.Duration) []byte {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return data
}

func unmarshalFrame(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var frame map[string]any
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatalf("unmarshal frame %s: %v", data, err)
	}
	return frame
}

func readJSON(t *testing.T, conn *websocket.Conn, timeout time.Duration) map[string]any {
	t.Helper()
	return unmarshalFrame(t, readRaw(t, conn, timeout))
}

// waitForEvent reads client frames until one of the wanted type arrives,
// failing on the timeout.
func waitForEvent(t *testing.T, conn *websocket.Conn, want string, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("timed out waiting for %s event", want)
		}
		ev := readJSON(t, conn, remaining)
		if eventType(ev) == want {
			return ev
		}
	}
}

// waitFor polls until cond() is true or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// deepEqual compares two decoded frames structurally; "" means equal.
func deepEqual(got, want map[string]any) string {
	if reflect.DeepEqual(got, want) {
		return ""
	}
	gb, _ := json.Marshal(got)
	wb, _ := json.Marshal(want)
	return string(gb) + " != " + string(wb)
}

func dialWS(t *testing.T, base, query string, header http.Header) *websocket.Conn {
	t.Helper()
	u := strings.Replace(base, "http", "ws", 1) + "/v1/realtime"
	if query != "" {
		vals, err := url.ParseQuery(query)
		if err != nil {
			t.Fatalf("parse query %q: %v", query, err)
		}
		// Re-encode: raw values with spaces would make the request line
		// invalid HTTP and the upgrade would fail with a bad handshake.
		u += "?" + vals.Encode()
	}
	dialer := &websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, _, err := dialer.Dial(u, header)
	if err != nil {
		t.Fatalf("dial client ws: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// closeInfo is one received WebSocket close frame.
type closeInfo struct {
	code int
	text string
}

// readClose drains frames until the close frame arrives and returns it.
func readClose(t *testing.T, conn *websocket.Conn) closeInfo {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, _, err := conn.ReadMessage()
		if err == nil {
			continue
		}
		var ce *websocket.CloseError
		if errors.As(err, &ce) {
			return closeInfo{ce.Code, ce.Text}
		}
		t.Fatalf("read close: %v", err)
	}
}

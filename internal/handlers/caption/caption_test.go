// Test helpers: a temp-SQLite repo, fake upstream WebSocket servers
// (httptest + gorilla Upgrader, per internal/handlers/media/gemini_live_stt_test.go),
// and client-side frame readers with deadlines.
package caption

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"9router/proxy/internal/db"

	"github.com/gorilla/websocket"
)

func newTestRepo(t *testing.T) *db.Repo {
	t.Helper()
	conn, err := db.OpenDatabase(t.TempDir() + "/caption.sqlite")
	if err != nil {
		t.Fatalf("OpenDatabase: %v", err)
	}
	if err := db.EnsureCoreSchema(conn); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return db.NewRepo(conn)
}

// newTestHandler builds a handler whose capability gate accepts every
// provider (the real gate is Lane A's catalog; gate behaviour is covered by
// its own test that stubs it false).
func newTestHandler(t *testing.T) *Handler {
	t.Helper()
	h := NewHandler(newTestRepo(t))
	h.supported = func(string) bool { return true }
	return h
}

// addConnection seeds one active provider connection carrying dataJSON.
func addConnection(t *testing.T, repo *db.Repo, id, provider, dataJSON string) {
	t.Helper()
	if err := repo.CreateProviderConnectionFull(id, provider, "api_key", id+"-name", nil, dataJSON); err != nil {
		t.Fatalf("create connection: %v", err)
	}
}

// serveHandler mounts h.HandleWS on an httptest server and returns the
// websocket URL of the caption path.
func serveHandler(t *testing.T, h *Handler) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(h.HandleWS))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// dialClient opens the client side of the caption session with the query.
func dialClient(t *testing.T, base, query string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(base+"/v1/audio/live-transcriptions?"+query, nil)
	if err != nil {
		t.Fatalf("dial client: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// readFrame reads the next gateway text frame with a deadline.
func readFrame(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	mt, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if mt != websocket.TextMessage {
		t.Fatalf("frame type = %d, want text", mt)
	}
	var frame map[string]any
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatalf("decode frame %q: %v", data, err)
	}
	return frame
}

// expectFrameType asserts the next frame's type and returns it.
func expectFrameType(t *testing.T, conn *websocket.Conn, want string) map[string]any {
	t.Helper()
	frame := readFrame(t, conn)
	if got, _ := frame["type"].(string); got != want {
		t.Fatalf("frame type = %q, want %q (%v)", got, want, frame)
	}
	return frame
}

// expectTimeout reads until a short deadline expecting silence.
func expectTimeout(t *testing.T, conn *websocket.Conn, d time.Duration) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(d)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatalf("expected no frame within %s", d)
	}
}

// expectCloseError reads until the socket closes, asserting the close code.
func expectCloseError(t *testing.T, conn *websocket.Conn, wantCode int) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	for {
		_, _, err := conn.ReadMessage()
		if err == nil {
			continue // drain any leading frames
		}
		var closeErr *websocket.CloseError
		if !asClose(err, &closeErr) {
			t.Fatalf("read error = %v, want close %d", err, wantCode)
		}
		if closeErr.Code != wantCode {
			t.Fatalf("close code = %d, want %d", closeErr.Code, wantCode)
		}
		return
	}
}

func asClose(err error, target **websocket.CloseError) bool {
	return errors.As(err, target)
}

// fakeUpstream is a fake provider WebSocket: it records the upgrade request
// and lets a test script drive frames.
type fakeUpstream struct {
	srv    *httptest.Server
	script func(conn *websocket.Conn)

	mu      sync.Mutex
	query   url.Values
	headers http.Header
	rec     map[string]any
}

func (f *fakeUpstream) wsURL() string {
	return "ws" + strings.TrimPrefix(f.srv.URL, "http")
}

func (f *fakeUpstream) upgradeQuery() url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.query
}

func (f *fakeUpstream) upgradeHeader(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.headers.Get(key)
}

// store records a decoded frame or detail for the test to assert later.
func (f *fakeUpstream) store(key string, v any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rec == nil {
		f.rec = map[string]any{}
	}
	f.rec[key] = v
}

// recorded returns the stored value for key, or nil.
func (f *fakeUpstream) recorded(key string) any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rec[key]
}

// waitForUpgrade polls until the fake upstream has recorded its upgrade
// (the gateway dials asynchronously after `connected`).
func waitForUpgrade(t *testing.T, f *fakeUpstream) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if f.upgradeQuery() != nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("upstream was never dialed")
}

// waitForStored polls until key is recorded or the deadline passes.
func waitForStored(t *testing.T, f *fakeUpstream, key string, d time.Duration) any {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if v := f.recorded(key); v != nil {
			return v
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("upstream never recorded %q", key)
	return nil
}

// sendJSON writes one text frame to the gateway from the fake script.
func sendJSON(conn *websocket.Conn, v any) error {
	return conn.WriteJSON(v)
}

func newFakeUpstream(t *testing.T, script func(conn *websocket.Conn)) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{script: script}
	upgrader := websocket.Upgrader{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Record before Upgrade: the gateway's dial returns the moment the
		// 101 is written, which can beat the post-upgrade lines below.
		f.mu.Lock()
		f.query = r.URL.Query()
		f.headers = r.Header.Clone()
		f.mu.Unlock()
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		script(conn)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// idleScript reads until the socket dies, as an upstream that only receives
// audio would.
func idleScript(conn *websocket.Conn) {
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

// usageCount polls the usageHistory table until it holds want rows for the
// caption endpoint, returning the latest row.
func usageCount(t *testing.T, repo *db.Repo, wantRows int) promptRow {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := repo.RawDB().Query(
			`SELECT promptTokens, completionTokens FROM usageHistory WHERE endpoint = ?`, captionEndpoint)
		if err != nil {
			t.Fatalf("query usage: %v", err)
		}
		var found []promptRow
		for rows.Next() {
			var r promptRow
			_ = rows.Scan(&r.Prompt, &r.Completion)
			found = append(found, r)
		}
		rows.Close()
		if len(found) >= wantRows {
			if len(found) != wantRows {
				return promptRow{Prompt: -1, Completion: -1, Count: len(found)}
			}
			return promptRow{Prompt: found[0].Prompt, Completion: found[0].Completion, Count: len(found)}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return promptRow{Prompt: -1, Completion: -1, Count: 0}
}

type promptRow struct {
	Prompt     int
	Completion int
	Count      int
}

func (r promptRow) String() string {
	return fmt.Sprintf("count=%d prompt=%d completion=%d", r.Count, r.Prompt, r.Completion)
}

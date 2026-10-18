// Session tests for the selfhosted fallback backend and the REST endpoints.
package caption

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
)

// newSelfhostedEnv wires a fake OpenAI-compatible transcription endpoint and
// a connection whose baseUrl override points at it.
func newSelfhostedEnv(t *testing.T, apiKey string) (*Handler, *fakeREST, *websocket.Conn) {
	t.Helper()
	rest := newFakeREST(t)
	h := newTestHandler(t)
	h.selfhostedBase = "http://127.0.0.1:1/unreachable" // proves the override wins
	addConnection(t, h.repo, "sh-1", "acme-stt", `{"apiKey":"`+apiKey+`","baseUrl":"`+rest.srv.URL+`"}`)
	h.supported = func(provider string) bool { return provider == "acme-stt" || provider == "deepgram" }
	conn := dialClient(t, serveHandler(t, h), "model=acme-stt/whisper-1")
	return h, rest, conn
}

func TestCaptionSessionSelfhosted(t *testing.T) {
	t.Run("ready is sent without a socket dial and audio buffers to 64 KiB", func(t *testing.T) {
		h, rest, conn := newSelfhostedEnv(t, "sh-key")
		expectFrameType(t, conn, "connected")
		expectFrameType(t, conn, "ready")
		if rest.Requests() != 0 {
			t.Fatalf("no POST expected before the buffer fills")
		}

		// 2 seconds of 16 kHz 16-bit mono exceeds the flush window.
		if err := conn.WriteMessage(websocketBinary, make([]byte, selfhostedFlushBytes)); err != nil {
			t.Fatalf("send audio: %v", err)
		}
		word := expectFrameType(t, conn, "word")
		if word["word"] != "halo" || word["isFinal"] != true {
			t.Errorf("flushed word = %v", word)
		}

		req := rest.LastRequest()
		if req.Method != http.MethodPost {
			t.Fatalf("flush method = %s, want POST", req.Method)
		}
		if req.Header.Get("Content-Type") == "" || !strings.Contains(req.Header.Get("Content-Type"), "multipart/form-data") {
			t.Errorf("flush content type = %q, want multipart", req.Header.Get("Content-Type"))
		}
		if req.Header.Get("Authorization") != "Bearer sh-key" {
			t.Errorf("flush authorization = %q, want Bearer sh-key", req.Header.Get("Authorization"))
		}
		if rest.LastFields()["model"] != "whisper-1" || rest.LastFields()["response_format"] != "verbose_json" || rest.LastFields()["language"] != "id" {
			t.Errorf("flush fields = %v", rest.LastFields())
		}
		if len(rest.LastAudio()) != selfhostedFlushBytes {
			t.Errorf("flushed audio = %d bytes, want %d", len(rest.LastAudio()), selfhostedFlushBytes)
		}
		_ = h
	})

	t.Run("finalize flushes the remainder of the buffer", func(t *testing.T) {
		_, rest, conn := newSelfhostedEnv(t, "sh-key")
		expectFrameType(t, conn, "connected")
		expectFrameType(t, conn, "ready")
		if err := conn.WriteMessage(websocketBinary, []byte("short")); err != nil {
			t.Fatalf("send audio: %v", err)
		}
		if err := conn.WriteJSON(map[string]any{"type": "finalize"}); err != nil {
			t.Fatalf("send finalize: %v", err)
		}
		word := expectFrameType(t, conn, "word")
		if word["word"] != "halo" {
			t.Errorf("flushed word = %v", word)
		}
		// The word frame only appears after the finalize flush, so exactly
		// one POST has happened by now.
		if rest.Requests() != 1 {
			t.Errorf("POSTs = %d, want 1", rest.Requests())
		}
		if len(rest.LastAudio()) != len("short") {
			t.Errorf("flushed audio = %d bytes, want 5", len(rest.LastAudio()))
		}
	})

	t.Run("a key of none omits the authorization header", func(t *testing.T) {
		_, rest, conn := newSelfhostedEnv(t, "none")
		expectFrameType(t, conn, "connected")
		expectFrameType(t, conn, "ready")
		if err := conn.WriteMessage(websocketBinary, make([]byte, selfhostedFlushBytes)); err != nil {
			t.Fatalf("send audio: %v", err)
		}
		expectFrameType(t, conn, "word")
		if req := rest.LastRequest(); req.Header.Get("Authorization") != "" {
			t.Errorf("authorization = %q, want it absent for key none", req.Header.Get("Authorization"))
		}
	})
}

// websocketBinary aliases the constant for the local tests' readability.
const websocketBinary = websocket.BinaryMessage

// fakeREST records OpenAI-compatible transcription POSTs.
type fakeREST struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests int
	lastReq  *http.Request
	fields   map[string]string
	audio    []byte
}

func (f *fakeREST) Requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

func (f *fakeREST) LastRequest() *http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastReq
}

func (f *fakeREST) LastFields() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fields
}

func (f *fakeREST) LastAudio() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.audio
}

func newFakeREST(t *testing.T) *fakeREST {
	t.Helper()
	f := &fakeREST{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(4 << 20); err != nil {
			http.Error(w, "bad multipart", http.StatusBadRequest)
			return
		}
		fields := map[string]string{
			"model":           r.FormValue("model"),
			"response_format": r.FormValue("response_format"),
			"language":        r.FormValue("language"),
		}
		var audio []byte
		if file, _, err := r.FormFile("file"); err == nil {
			audio, _ = io.ReadAll(file)
			file.Close()
		}

		f.mu.Lock()
		f.requests++
		f.lastReq = r
		f.fields = fields
		f.audio = audio
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"words": []map[string]any{{"word": "halo", "start": 0.0, "end": 0.4}},
		})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func TestCaptionEndpointsWithoutUpgrade(t *testing.T) {
	t.Run("GET without an upgrade header returns the protocol info document", func(t *testing.T) {
		h := newTestHandler(t)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/audio/live-transcriptions", nil)
		h.HandleWS(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Errorf("ACAO = %q, want *", rec.Header().Get("Access-Control-Allow-Origin"))
		}
		var doc map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("decode info doc: %v", err)
		}
		if doc["status"] != "ready" || doc["protocol"] != "websocket" {
			t.Errorf("info doc = %v", doc)
		}
		encodings, _ := doc["supportedEncodings"].([]any)
		if len(encodings) != 3 {
			t.Errorf("supportedEncodings = %v, want three entries", encodings)
		}
	})

	t.Run("POST is answered with 426 and the upgrade hint", func(t *testing.T) {
		h := newTestHandler(t)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/audio/live-transcriptions", nil)
		h.HandleUpgradeRequired(rec, req)
		if rec.Code != http.StatusUpgradeRequired {
			t.Fatalf("status = %d, want 426", rec.Code)
		}
		if rec.Header().Get("Upgrade") != "WebSocket" || rec.Header().Get("Connection") != "Upgrade" {
			t.Errorf("upgrade hint headers = %q / %q", rec.Header().Get("Upgrade"), rec.Header().Get("Connection"))
		}
		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode 426 body: %v", err)
		}
		if !strings.Contains(body["error"], "live-transcriptions requires a WebSocket connection") {
			t.Errorf("426 body = %v", body)
		}
	})
}

package media

import (
	"bytes"
	"database/sql"
	json "encoding/json/v2"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"9router/proxy/internal/db"
)

// The image edit lane: the caller attaches a photo plus an instruction, the
// gateway re-spools the multipart upload (rewriting only the model part) and
// streams it to the provider's /v1/images/edits with the connection's key.

func seedKenariConn(t *testing.T, database *sql.DB, baseURL string) {
	t.Helper()
	seedOpenAIConnForProvider(t, database, "kenari", "sk-kenari", baseURL, "conn-kenari")
}

func seedOpenAIConnForProvider(t *testing.T, database *sql.DB, provider, key, baseURL, id string) {
	t.Helper()
	data, _ := json.Marshal(map[string]any{"apiKey": key, "baseUrl": baseURL})
	if _, err := database.Exec(`INSERT INTO providerConnections (id, provider, authType, name, priority, isActive, data, createdAt, updatedAt) VALUES
		(?, ?, 'apikey', ?, 1, 1, ?, '2026-10-27T00:00:00Z', '2026-10-27T00:00:00Z')`, id, provider, provider+" Test", string(data)); err != nil {
		t.Fatalf("seed %s connection: %v", provider, err)
	}
}

func buildImageEditBody(t *testing.T, model, imageBytes string, extraFields ...string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if model != "" {
		mw.WriteField("model", model)
	}
	mw.WriteField("prompt", "make the background blue")
	for i := 0; i+1 < len(extraFields); i += 2 {
		mw.WriteField(extraFields[i], extraFields[i+1])
	}
	if imageBytes != "" {
		fw, err := mw.CreateFormFile("image", "photo.png")
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		if _, err := io.Copy(fw, strings.NewReader(imageBytes)); err != nil {
			t.Fatalf("write form file: %v", err)
		}
	}
	mw.Close()
	return &buf, mw.FormDataContentType()
}

func callImageEdits(handler *MediaHandler, body io.Reader, contentType string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/v1/images/edits", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	handler.HandleImageEdits(rec, req)
	return rec
}

func TestHandleImageEdits_Success(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/edits" {
			t.Errorf("expected path /v1/images/edits, got %s", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer sk-kenari" {
			t.Errorf("expected Bearer sk-kenari, got %q", auth)
		}
		mr, err := r.MultipartReader()
		if err != nil {
			t.Errorf("upstream multipart: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		gotModel, gotPrompt, gotImage := "", "", ""
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("read part: %v", err)
				return
			}
			data, _ := io.ReadAll(p)
			switch p.FormName() {
			case "model":
				gotModel = string(data)
			case "prompt":
				gotPrompt = string(data)
			case "image":
				gotImage = string(data)
			}
		}
		// The provider prefix is stripped; the upstream id reaches the vendor.
		if gotModel != "gpt-image-2" {
			t.Errorf("upstream model = %q, want gpt-image-2", gotModel)
		}
		if gotPrompt != "make the background blue" {
			t.Errorf("upstream prompt = %q", gotPrompt)
		}
		if gotImage != "PNG-bytes" {
			t.Errorf("upstream image bytes = %q", gotImage)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"data":[{"url":"https://example.com/edited.png"}]}`))
	}))
	defer upstream.Close()

	database, cleanup := setupEmbeddingsTestDB(t)
	defer cleanup()
	seedKenariConn(t, database, upstream.URL)

	repo := db.NewRepo(database)
	handler := newTestMediaHandler(repo)

	body, ct := buildImageEditBody(t, "kenari/gpt-image-2", "PNG-bytes")
	rec := callImageEdits(handler, body, ct)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected upstream content-type passthrough, got %q", ct)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if data, _ := resp["data"].([]any); len(data) != 1 {
		t.Errorf("expected 1 image in response, got %v", resp)
	}
}

func TestHandleImageEdits_UploadTooLarge(t *testing.T) {
	database, cleanup := setupEmbeddingsTestDB(t)
	defer cleanup()
	repo := db.NewRepo(database)
	handler := newTestMediaHandler(repo)

	oversized := strings.Repeat("x", int(imageEditsMaxUpload)+1024)
	body, ct := buildImageEditBody(t, "kenari/gpt-image-2", oversized)
	rec := callImageEdits(handler, body, ct)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse error body: %v", err)
	}
	if resp["error"] == nil {
		t.Errorf("expected house error shape, got %v", resp)
	}
}

func TestHandleImageEdits_UnknownModel(t *testing.T) {
	database, cleanup := setupEmbeddingsTestDB(t)
	defer cleanup()
	repo := db.NewRepo(database)
	handler := newTestMediaHandler(repo)

	body, ct := buildImageEditBody(t, "nosuchprovider/gpt-image-2", "PNG-bytes")
	rec := callImageEdits(handler, body, ct)

	// Same error the generations path gives: an unresolvable model never
	// reaches an upstream call and fails at connection selection instead.
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no active connections for provider: nosuchprovider") {
		t.Errorf("expected the generations-path connection error, got %q", rec.Body.String())
	}
}

func TestHandleImageEdits_MissingImage(t *testing.T) {
	database, cleanup := setupEmbeddingsTestDB(t)
	defer cleanup()
	repo := db.NewRepo(database)
	handler := newTestMediaHandler(repo)

	body, ct := buildImageEditBody(t, "kenari/gpt-image-2", "")
	rec := callImageEdits(handler, body, ct)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing image part, got %d", rec.Code)
	}
}

func TestHandleImageEdits_UpstreamErrorPassthrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"image too small"}}`))
	}))
	defer upstream.Close()

	database, cleanup := setupEmbeddingsTestDB(t)
	defer cleanup()
	seedKenariConn(t, database, upstream.URL)

	repo := db.NewRepo(database)
	handler := newTestMediaHandler(repo)

	body, ct := buildImageEditBody(t, "kenari/gpt-image-2", "PNG-bytes")
	rec := callImageEdits(handler, body, ct)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected upstream status passthrough, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "image too small") {
		t.Errorf("expected upstream error body passthrough, got %q", rec.Body.String())
	}
}

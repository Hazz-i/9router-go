package media

import (
	"database/sql"
	json "encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"9router/proxy/internal/db"
)

// A custom-image node routes /v1/images/generations exactly like
// custom-embedding routes /v1/embeddings: the node's baseUrl — with or
// without its own /v1 — must produce exactly one version segment upstream,
// never "/v1/v1/images/generations".
func TestHandleImages_CustomImageNodeBaseURL(t *testing.T) {
	tests := []struct {
		name   string
		baseFn func(host string) string
	}{
		{name: "bare host", baseFn: func(host string) string { return host }},
		{name: "host with /v1", baseFn: func(host string) string { return host + "/v1" }},
		{name: "host with /v1/", baseFn: func(host string) string { return host + "/v1/" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/images/generations" {
					t.Errorf("expected path /v1/images/generations, got %s", r.URL.Path)
				}
				if auth := r.Header.Get("Authorization"); auth != "Bearer sk-kenari" {
					t.Errorf("expected Bearer sk-kenari, got %q", auth)
				}
				var reqBody struct {
					Model string `json:"model"`
				}
				if err := json.UnmarshalRead(r.Body, &reqBody); err != nil {
					t.Errorf("decode upstream body: %v", err)
				} else if reqBody.Model != "flux-mini" {
					t.Errorf("expected rewritten model flux-mini, got %q", reqBody.Model)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"created":1,"data":[{"url":"https://example.com/img.png"}]}`))
			}))
			defer upstream.Close()

			base := tt.baseFn(upstream.URL)
			database, cleanup := setupEmbeddingsTestDB(t)
			defer cleanup()
			seedCustomImageNode(t, database, "custom-image-kenari", "kenari", base)

			repo := db.NewRepo(database)
			handler := newTestMediaHandler(repo)

			body := `{"model":"kenari/flux-mini","prompt":"a cat"}`
			req := httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			handler.HandleImages(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// seedCustomImageNode seeds a custom-image provider node and one active
// connection attached to it, the way the dashboard creates them.
func seedCustomImageNode(t *testing.T, database *sql.DB, nodeID, prefix, baseURL string) {
	t.Helper()
	if _, err := database.Exec(`CREATE TABLE IF NOT EXISTS providerNodes (
			id TEXT PRIMARY KEY,
			type TEXT,
			name TEXT,
			data TEXT NOT NULL,
			createdAt TEXT NOT NULL,
			updatedAt TEXT NOT NULL
		)`); err != nil {
		t.Fatalf("create providerNodes table: %v", err)
	}
	nodeData, _ := json.Marshal(map[string]string{"prefix": prefix, "baseUrl": baseURL})
	if _, err := database.Exec(`INSERT INTO providerNodes (id, type, name, data, createdAt, updatedAt) VALUES
		(?, 'custom-image', ?, ?, '2026-07-18T00:00:00Z', '2026-07-18T00:00:00Z')`,
		nodeID, "Kenari", string(nodeData)); err != nil {
		t.Fatalf("seed provider node: %v", err)
	}
	connData, _ := json.Marshal(map[string]any{
		"apiKey":               "sk-kenari",
		"baseUrl":              baseURL,
		"providerSpecificData": map[string]any{"prefix": prefix, "baseUrl": baseURL},
	})
	if _, err := database.Exec(`INSERT INTO providerConnections (id, provider, authType, name, priority, isActive, data, createdAt, updatedAt) VALUES
		('conn-kenari', ?, 'apikey', 'Kenari', 1, 1, ?, '2026-07-18T00:00:00Z', '2026-07-18T00:00:00Z')`,
		nodeID, string(connData)); err != nil {
		t.Fatalf("seed connection: %v", err)
	}
}

package realtime

import (
	"database/sql"
	"encoding/base64"
	json "encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"9router/proxy/internal/db"
	"github.com/gorilla/websocket"
)

// TestHandleWS_GetInfoDocument: a plain GET returns the protocol-info document.
func TestHandleWS_GetInfoDocument(t *testing.T) {
	_, repo := newTestRepo(t)
	gateway := newGateway(t, repo, nil)

	resp, err := http.Get(gateway.URL + "/v1/realtime")
	if err != nil {
		t.Fatalf("GET info: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("ACAO = %q, want *", resp.Header.Get("Access-Control-Allow-Origin"))
	}
	var doc map[string]any
	var body []byte
	_, body, err = readAll(resp)
	if err == nil {
		err = json.Unmarshal(body, &doc)
	}
	if err != nil {
		t.Fatalf("decode info doc: %v", err)
	}
	if doc["status"] != "ready" || doc["protocol"] != "websocket" ||
		doc["defaultModel"] != "gemini-3.8-live" ||
		doc["protocolFormat"] != "OpenAI Realtime API compatible" {
		t.Errorf("info doc = %v", doc)
	}
}

// TestHandleWS_PostRequiresUpgrade: POST answers 426 with the Upgrade headers.
func TestHandleWS_PostRequiresUpgrade(t *testing.T) {
	_, repo := newTestRepo(t)
	gateway := newGateway(t, repo, nil)

	resp, err := http.Post(gateway.URL+"/v1/realtime", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("status = %d, want 426", resp.StatusCode)
	}
	if resp.Header.Get("Upgrade") != "WebSocket" || resp.Header.Get("Connection") != "Upgrade" {
		t.Errorf("headers = %v, want Upgrade: WebSocket / Connection: Upgrade", resp.Header)
	}
	var body map[string]string
	var raw []byte
	_, raw, err = readAll(resp)
	if err == nil {
		err = json.Unmarshal(raw, &body)
	}
	if err != nil {
		t.Fatalf("decode 426 body: %v", err)
	}
	if !strings.Contains(body["error"], "requires a WebSocket connection") {
		t.Errorf("426 body = %v", body)
	}
}

// startPlainGateway is a gateway without any seeded connection.
func startPlainGateway(t *testing.T, settings map[string]any) (*sql.DB, *httptest.Server) {
	t.Helper()
	database, repo := newTestRepo(t)
	if settings != nil {
		blob, _ := json.Marshal(settings)
		if _, err := database.Exec(`UPDATE settings SET data = ? WHERE id = 1`, string(blob)); err != nil {
			t.Fatalf("seed settings: %v", err)
		}
	}
	gateway := newGateway(t, repo, nil)
	return database, gateway
}

// TestHandleWS_AuthFailureCloseCode: with requireApiKey on, a missing/invalid
// token gets the error event then close 4401 "Unauthorized" — before any
// provider work happens.
func TestHandleWS_AuthFailureCloseCode(t *testing.T) {
	database, gateway := startPlainGateway(t, map[string]any{"requireApiKey": true})
	seedClientAPIKey(t, database, testAPIKey)

	for _, tt := range []struct {
		name   string
		query  string
		header http.Header
	}{
		{name: "no token at all", query: ""},
		{name: "wrong token in query", query: "apiKey=sk-wrong"},
		{name: "wrong bearer header", query: "", header: http.Header{"Authorization": []string{"Bearer sk-wrong"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := dialWS(t, gateway.URL, tt.query, tt.header)
			ev := readJSON(t, client, 3*time.Second)
			if eventType(ev) != "error" {
				t.Fatalf("expected an error event, got %v", ev)
			}
			errBody := ev["error"].(map[string]any)
			if errBody["code"] != "unauthorized" {
				t.Errorf("error code = %v, want unauthorized", errBody["code"])
			}
			ci := readClose(t, client)
			if ci.code != 4401 || ci.text != "Unauthorized" {
				t.Errorf("close = %d/%q, want 4401/Unauthorized", ci.code, ci.text)
			}
		})
	}

	t.Run("valid query token opens the session far enough to fail on credentials", func(t *testing.T) {
		t.Setenv("GEMINI_API_KEY", "")
		client := dialWS(t, gateway.URL, "apiKey="+testAPIKey+"&model=gemini/gemini-3.8-live", nil)
		ev := readJSON(t, client, 3*time.Second)
		errBody := ev["error"].(map[string]any)
		// No gemini connection exists, so auth passes and credential
		// resolution is the first failure — proving the query token was read.
		if errBody["code"] != "provider_missing_credentials" {
			t.Fatalf("error code = %v, want provider_missing_credentials", errBody["code"])
		}
		if ci := readClose(t, client); ci.code != 4401 {
			t.Errorf("close code = %d, want 4401", ci.code)
		}
	})
}

// TestHandleWS_UnsupportedProvider: a provider without a realtimeConfig fails
// as provider_unsupported + close 4400, before any upstream dial.
func TestHandleWS_UnsupportedProvider(t *testing.T) {
	_, gateway := startPlainGateway(t, nil)
	client := dialWS(t, gateway.URL, "model=openai/gpt-x", nil)

	ev := readJSON(t, client, 3*time.Second)
	errBody := ev["error"].(map[string]any)
	if errBody["code"] != "provider_unsupported" {
		t.Errorf("error code = %v, want provider_unsupported", errBody["code"])
	}
	if !strings.Contains(errBody["message"].(string), "does not support realtime voice") {
		t.Errorf("message = %v", errBody["message"])
	}
	if ci := readClose(t, client); ci.code != 4400 {
		t.Errorf("close code = %d, want 4400", ci.code)
	}
}

// TestHandleWS_MissingProviderCredentials: a supported provider with no
// configured connection fails as provider_missing_credentials + 4401.
func TestHandleWS_MissingProviderCredentials(t *testing.T) {
	_, gateway := startPlainGateway(t, nil)
	client := dialWS(t, gateway.URL, "model=gemini/gemini-3.8-live", nil)

	ev := readJSON(t, client, 3*time.Second)
	errBody := ev["error"].(map[string]any)
	if errBody["code"] != "provider_missing_credentials" {
		t.Errorf("error code = %v, want provider_missing_credentials", errBody["code"])
	}
	if ci := readClose(t, client); ci.code != 4401 || ci.text != "Missing provider credentials" {
		t.Errorf("close = %d/%q, want 4401/Missing provider credentials", ci.code, ci.text)
	}
}

// TestUsageRowWrittenOnceOnClose: one session writes exactly one usageHistory
// row with the 25-tokens-per-second estimate, the configured rates, endpoint
// "/v1/realtime", cost 0 and status success.
func TestUsageRowWrittenOnceOnClose(t *testing.T) {
	fake := newFakeUpstream(t, func(t *testing.T, conn *websocket.Conn) {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, _, err := conn.ReadMessage(); err != nil { // setup
			return
		}
		writeJSON(t, conn, map[string]any{"setupComplete": map[string]any{}})
		// 48000 bytes = 1.0 s of PCM16 @24 kHz → 25 completion tokens. Audio
		// output rides the JSON modelTurn envelope (Gemini never sends raw
		// binary upstream frames).
		audioB64 := base64.StdEncoding.EncodeToString(make([]byte, 24000*2))
		writeJSON(t, conn, map[string]any{"serverContent": map[string]any{
			"modelTurn": map[string]any{"parts": []any{
				map[string]any{"inlineData": map[string]any{"data": audioB64}},
			}},
		}})
		writeJSON(t, conn, map[string]any{"serverContent": map[string]any{"turnComplete": true}})
		time.Sleep(400 * time.Millisecond) // let the client read the turn
	})
	database, repo := newTestRepo(t)
	seedConnection(t, database, "gemini", "gk-1", map[string]any{"baseUrl": fake.URL})
	gateway := newGateway(t, repo, map[string]string{"gemini": "wss://real-gemini.invalid/ws"})

	client := dialWS(t, gateway.URL, "", nil)
	fake.open()
	waitForEvent(t, client, "session.created", 3*time.Second)
	// 32000 bytes = 1.0 s of PCM16 @16 kHz → 25 prompt tokens.
	if err := client.WriteMessage(websocket.BinaryMessage, make([]byte, 16000*2)); err != nil {
		t.Fatalf("client audio: %v", err)
	}
	waitForEvent(t, client, "response.done", 3*time.Second)
	_ = client.Close() // end the session

	waitFor(t, 3*time.Second, "usage row", func() bool { return usageRowCount(database) == 1 })
	time.Sleep(200 * time.Millisecond) // would-be second write lands here
	if n := usageRowCount(database); n != 1 {
		t.Errorf("usage rows = %d, want exactly 1", n)
	}

	row := queryUsageRow(t, database)
	if row.Provider != "gemini" || row.Model != "gemini-3.8-live" {
		t.Errorf("provider/model = %q/%q", row.Provider, row.Model)
	}
	if row.Endpoint != "/v1/realtime" {
		t.Errorf("endpoint = %q, want /v1/realtime", row.Endpoint)
	}
	if row.PromptTokens != 25 || row.CompletionTokens != 25 {
		t.Errorf("tokens = %d/%d, want 25/25 (25 tok per second of PCM)", row.PromptTokens, row.CompletionTokens)
	}
	if row.Cost != 0 || row.Status != "success" || row.ConnectionID != "conn-rt" {
		t.Errorf("row = %+v", row)
	}
}

// TestHandleWS_UpstreamCloseClosesClientWith1000: when the upstream closes
// while the client is still open, the client gets 1000 "Upstream closed".
func TestHandleWS_UpstreamCloseClosesClientWith1000(t *testing.T) {
	fake := newFakeUpstream(t, func(t *testing.T, conn *websocket.Conn) {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, _, err := conn.ReadMessage(); err != nil { // setup
			return
		}
		writeJSON(t, conn, map[string]any{"setupComplete": map[string]any{}})
		time.Sleep(300 * time.Millisecond) // client reads the session events
		// Script returns → the fake closes the upstream socket.
	})
	database, repo := newTestRepo(t)
	seedConnection(t, database, "gemini", "gk-1", map[string]any{"baseUrl": fake.URL})
	gateway := newGateway(t, repo, map[string]string{"gemini": "wss://real-gemini.invalid/ws"})

	client := dialWS(t, gateway.URL, "", nil)
	fake.open()
	waitForEvent(t, client, "session.created", 3*time.Second)

	ci := readClose(t, client)
	if ci.code != 1000 || ci.text != "Upstream closed" {
		t.Errorf("close = %d/%q, want 1000/Upstream closed", ci.code, ci.text)
	}
}

// --- small helpers -----------------------------------------------------------

// readAll drains an HTTP response body: [resp, body, error].
func readAll(resp *http.Response) (*http.Response, []byte, error) {
	b, err := io.ReadAll(resp.Body)
	return resp, b, err
}

func seedClientAPIKey(t *testing.T, database *sql.DB, key string) {
	t.Helper()
	if _, err := database.Exec(
		`INSERT INTO apiKeys (id, key, isActive, createdAt) VALUES ('key-1', ?, 1, '2026-07-18T00:00:00Z')`, key); err != nil {
		t.Fatalf("seed api key: %v", err)
	}
}

func usageRowCount(database *sql.DB) int {
	var n int
	_ = database.QueryRow(`SELECT COUNT(*) FROM usageHistory`).Scan(&n)
	return n
}

func queryUsageRow(t *testing.T, database *sql.DB) db.UsageHistoryRow {
	t.Helper()
	rows, err := database.Query(`SELECT timestamp, COALESCE(provider,''), COALESCE(model,''), COALESCE(connectionId,''), COALESCE(apiKey,''), COALESCE(endpoint,''), COALESCE(promptTokens,0), COALESCE(completionTokens,0), COALESCE(cost,0.0), COALESCE(status,''), COALESCE(tokens,'{}') FROM usageHistory`)
	if err != nil {
		t.Fatalf("query usage rows: %v", err)
	}
	defer rows.Close()
	var row db.UsageHistoryRow
	for rows.Next() {
		if err := rows.Scan(&row.Timestamp, &row.Provider, &row.Model, &row.ConnectionID,
			&row.APIKey, &row.Endpoint, &row.PromptTokens, &row.CompletionTokens,
			&row.Cost, &row.Status, &row.Tokens); err != nil {
			t.Fatalf("scan usage row: %v", err)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate usage rows: %v", err)
	}
	return row
}

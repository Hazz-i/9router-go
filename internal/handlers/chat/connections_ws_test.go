package chat

import (
	"errors"
	"net/http"
	"net/url"
	"testing"

	"9router/proxy/internal/db"
	internalproxy "9router/proxy/internal/proxy"
)

// TestWebSocketDialerForConnection covers the whole chain a socket walks: the
// assigned pool, the legacy per-connection proxy, an edge relay that cannot
// tunnel, and the refusals a strict connection must produce.
func TestWebSocketDialerForConnection(t *testing.T) {
	database, cleanup := setupChatTestDB(t)
	defer cleanup()

	if _, err := database.Exec(`CREATE TABLE IF NOT EXISTS proxyPools (
		id TEXT PRIMARY KEY,
		isActive INTEGER DEFAULT 1,
		testStatus TEXT,
		data TEXT NOT NULL,
		createdAt TEXT NOT NULL,
		updatedAt TEXT NOT NULL
	);`); err != nil {
		t.Fatalf("failed to create proxyPools table: %v", err)
	}
	repo := db.NewRepo(database)
	h := &ChatHandler{Client: &http.Client{}, Repo: repo}

	insertPool := func(name, url, typ string, strict bool) string {
		t.Helper()
		pool, err := repo.InsertProxyPool(db.ProxyPoolData{
			Name: name, ProxyURL: url, Type: typ, StrictProxy: strict,
		})
		if err != nil {
			t.Fatalf("insert pool %s: %v", name, err)
		}
		return pool["id"].(string)
	}
	httpPool := insertPool("sg-http", "http://user:pass@proxy.example.com:8080", "http", false)
	strictPool := insertPool("sg-strict", "http://proxy.example.com:8080", "http", true)
	relayPool := insertPool("vercel-relay", "https://relay.vercel.app", "vercel", false)
	strictRelayPool := insertPool("strict-relay", "https://relay.vercel.app", "vercel", true)
	httpsPool := insertPool("tls-proxy", "https://proxy.example.com:8443", "http", false)

	// A request carrying a URL: ProxyFromEnvironment dereferences it.
	req := &http.Request{URL: &url.URL{Scheme: "https", Host: "upstream.example.com"}}

	tests := []struct {
		name      string
		connData  *ConnectionData
		wantProxy string // "" means the ambient/direct path
		wantErr   error  // non-nil: the error must wrap it
		anyErr    bool
	}{
		{name: "no connection data dials direct"},
		{
			name:      "legacy per-connection proxy is honoured",
			connData:  &ConnectionData{ConnectionProxyEnabled: true, ConnectionProxyURL: "http://127.0.0.1:8888"},
			wantProxy: "http://127.0.0.1:8888",
		},
		{
			name:      "http pool is tunnelled",
			connData:  &ConnectionData{ProxyPoolID: httpPool},
			wantProxy: "http://user:pass@proxy.example.com:8080",
		},
		{
			name:      "strict http pool still yields the tunnel",
			connData:  &ConnectionData{ProxyPoolID: strictPool},
			wantProxy: "http://proxy.example.com:8080",
		},
		{
			name:     "edge relay without strict dials direct",
			connData: &ConnectionData{ProxyPoolID: relayPool},
		},
		{
			name:     "strict edge relay is refused",
			connData: &ConnectionData{ProxyPoolID: strictRelayPool},
			wantErr:  internalproxy.ErrStrictProxyRequired,
		},
		{
			name:     "https proxy scheme is named, not left to the handshake",
			connData: &ConnectionData{ProxyPoolID: httpsPool},
			anyErr:   true,
		},
		{
			name:     "assigned pool that does not exist is refused",
			connData: &ConnectionData{ProxyPoolID: "nope"},
			anyErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := h.WebSocketDialerForConnection(tt.connData)
			if tt.wantErr != nil || tt.anyErr {
				if err == nil {
					t.Fatalf("expected an error, got dialer %+v", d)
				}
				if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
					t.Errorf("error should wrap %v, got: %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("WebSocketDialerForConnection: %v", err)
			}
			if d.NetDialContext == nil {
				t.Error("expected the SSRF-safe dial context to be installed")
			}
			if d.Proxy == nil {
				t.Fatal("expected a proxy hook, got nil")
			}
			u, err := d.Proxy(req)
			if err != nil {
				t.Fatalf("probe proxy: %v", err)
			}
			if tt.wantProxy == "" {
				// Nothing in the test environment names a proxy, so a nil result
				// here is the same direct path the HTTP client takes.
				if u != nil {
					t.Errorf("expected no proxy from the environment, got %v", u)
				}
				return
			}
			if u == nil || u.String() != tt.wantProxy {
				t.Errorf("proxy = %v, want %s", u, tt.wantProxy)
			}
		})
	}
}

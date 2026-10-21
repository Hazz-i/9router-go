package realtime

import (
	"errors"
	"net/http"
	"net/url"
	"testing"

	"9router/proxy/internal/db"
	"9router/proxy/internal/handlers/chat"
	"9router/proxy/internal/handlers/shared"
	internalproxy "9router/proxy/internal/proxy"
	"github.com/gorilla/websocket"
)

// mustURL parses a proxy url or fails the test; probeProxy compares through it.
func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url %q: %v", raw, err)
	}
	return u
}

// probeProxy returns the url a gorilla dialer's Proxy hook yields (nil when it
// dials without a proxy function).
func probeProxy(t *testing.T, d *websocket.Dialer) *url.URL {
	t.Helper()
	if d.Proxy == nil {
		return nil
	}
	u, err := d.Proxy(&http.Request{})
	if err != nil {
		t.Fatalf("probe proxy: %v", err)
	}
	if u == nil {
		return nil
	}
	return u
}

// TestDialerForConnection_ProxyPool proves the realtime lane actually consults
// the connection's proxy pool: an http pool is tunnelled, a strict edge relay
// refuses the session, and no wired dialFor keeps the direct dialer.
func TestDialerForConnection_ProxyPool(t *testing.T) {
	_, repo := newTestRepo(t)
	chatH := &chat.ChatHandler{Repo: repo, Client: &http.Client{}}
	h := NewHandler(repo, chatH)

	httpPool, err := repo.InsertProxyPool(db.ProxyPoolData{
		Name: "sg-http", ProxyURL: "http://user:pass@proxy.example.com:8080", Type: "http",
	})
	if err != nil {
		t.Fatalf("insert http pool: %v", err)
	}
	relayPool, err := repo.InsertProxyPool(db.ProxyPoolData{
		Name: "edge-relay", ProxyURL: "https://relay.example.com", Type: "vercel", StrictProxy: true,
	})
	if err != nil {
		t.Fatalf("insert relay pool: %v", err)
	}

	tests := []struct {
		name       string
		handler    *Handler
		connData   *shared.ConnectionData
		wantProxy  *url.URL // non-nil: the dialer's Proxy hook must yield it ("" URL: none)
		wantDirect bool     // expect the exact &realtimeDialer shared instance
		wantStrict bool
	}{
		{
			name:      "http proxy pool is tunnelled",
			connData:  &shared.ConnectionData{ProxyPoolID: httpPool["id"].(string)},
			wantProxy: mustURL(t, "http://user:pass@proxy.example.com:8080"),
		},
		{
			name:       "strict edge-relay pool refuses the session",
			connData:   &shared.ConnectionData{ProxyPoolID: relayPool["id"].(string)},
			wantStrict: true,
		},
		{
			name:       "nil dialFor ignores the pool and keeps the shared direct dialer",
			handler:    NewHandler(repo, nil),
			connData:   &shared.ConnectionData{ProxyPoolID: httpPool["id"].(string)},
			wantDirect: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := tt.handler
			if handler == nil {
				handler = h
			}
			d, err := handler.dialerFor(tt.connData)
			if tt.wantStrict {
				if err == nil {
					t.Fatalf("expected strict refusal, got dialer %+v", d)
				}
				if !errors.Is(err, internalproxy.ErrStrictProxyRequired) {
					t.Errorf("error should wrap ErrStrictProxyRequired, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("dialerFor: %v", err)
			}
			if tt.wantDirect {
				if d != &realtimeDialer {
					t.Errorf("expected the shared direct dialer, got %+v", d)
				}
				return
			}
			if tt.wantProxy == nil {
				t.Fatalf("test misconfigured: wantProxy or wantDirect must be set")
			}
			got := probeProxy(t, d)
			if got == nil || got.String() != tt.wantProxy.String() {
				t.Errorf("proxy = %v, want %s", got, tt.wantProxy)
			}
		})
	}
}

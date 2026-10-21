// Tests for the per-connection outbound-proxy dialer: the session dials its
// upstream through the chat lane's proxy chain, and a pool the WebSocket
// cannot use refuses the session before any upstream dial.
package caption

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"9router/proxy/internal/db"
	"9router/proxy/internal/handlers/chat"
)

const proxyTestProxyURL = "http://user:pass@proxy.example.com:8080"

// newProxyTestRepo seeds a repo with one active deepgram connection whose
// dataJSON carries the given proxyPoolId, returning the handler wired to a
// chat handler that shares the repo (the router.go wiring, in miniature).
func newProxyTestRepo(t *testing.T, dataJSON string) *Handler {
	t.Helper()
	repo := newTestRepo(t)
	addConnection(t, repo, "conn-1", "deepgram", dataJSON)
	return NewHandler(repo, &chat.ChatHandler{Repo: repo, Client: &http.Client{}})
}

// seedPool inserts a pool row and returns its id.
func seedPool(t *testing.T, repo *db.Repo, pd db.ProxyPoolData) string {
	t.Helper()
	pool, err := repo.InsertProxyPool(pd)
	if err != nil {
		t.Fatalf("insert proxy pool: %v", err)
	}
	return pool["id"].(string)
}

// setPoolActive flips isActive after the fact (InsertProxyPool always starts active).
func setPoolActive(t *testing.T, repo *db.Repo, poolID string, active bool) {
	t.Helper()
	on := 0
	if active {
		on = 1
	}
	if _, err := repo.RawDB().Exec(
		`UPDATE proxyPools SET isActive = ? WHERE id = ?`, on, poolID); err != nil {
		t.Fatalf("deactivate pool: %v", err)
	}
}

// newProxySession builds a bare session whose resolution runs without a
// client socket (only the refusal paths need one, per ResolveProxyRefusals).
func newProxySession(t *testing.T, h *Handler) *session {
	t.Helper()
	s := &session{h: h, provider: "deepgram", model: "nova-3"}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	t.Cleanup(s.cancel)
	return s
}

// TestResolveConnectionProxyDialer walks the successful resolution paths: a
// pool-bound connection must produce a session dialer whose Proxy yields the
// pool URL; a connection without a pool keeps a default dialer.
func TestResolveConnectionProxyDialer(t *testing.T) {
	cases := []struct {
		name        string
		build       func(t *testing.T) *Handler
		wantDialNil bool
		wantProxy   string // asserted when non-empty
	}{
		{
			name: "http pool dialer yields pool proxy",
			build: func(t *testing.T) *Handler {
				repo := newTestRepo(t)
				poolID := seedPool(t, repo, db.ProxyPoolData{
					Name: "sg-proxy", ProxyURL: proxyTestProxyURL, Type: "http",
				})
				addConnection(t, repo, "conn-1", "deepgram",
					`{"apiKey":"sk-test","proxyPoolId":"`+poolID+`"}`)
				return NewHandler(repo, &chat.ChatHandler{Repo: repo, Client: &http.Client{}})
			},
			wantProxy: proxyTestProxyURL,
		},
		{
			name: "no chat handler keeps default dialer",
			build: func(t *testing.T) *Handler {
				repo := newTestRepo(t)
				addConnection(t, repo, "conn-1", "deepgram", `{"apiKey":"sk-test"}`)
				return NewHandler(repo, nil)
			},
			wantDialNil: true,
		},
		{
			name: "direct connection under chat handler gets a default dialer",
			build: func(t *testing.T) *Handler {
				repo := newTestRepo(t)
				addConnection(t, repo, "conn-1", "deepgram", `{"apiKey":"sk-test"}`)
				return NewHandler(repo, &chat.ChatHandler{Repo: repo, Client: &http.Client{}})
			},
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			h := tt.build(t)
			s := newProxySession(t, h)
			if !s.resolveConnection() {
				t.Fatal("resolveConnection refused a credentialed connection")
			}
			if tt.wantDialNil && s.dial != nil {
				t.Fatalf("session dialer = %v, want nil (default dialer)", s.dial)
			}
			if !tt.wantDialNil && s.dial == nil {
				t.Fatal("session dialer not resolved")
			}
			if tt.wantProxy == "" {
				return
			}
			u, err := url.Parse("wss://api.deepgram.com/v1/listen")
			if err != nil {
				t.Fatalf("parse target: %v", err)
			}
			proxy, err := s.dial.Proxy(&http.Request{URL: u})
			if err != nil {
				t.Fatalf("dial proxy resolve: %v", err)
			}
			if proxy == nil || proxy.String() != tt.wantProxy {
				t.Errorf("proxy = %v, want %s", proxy, tt.wantProxy)
			}
		})
	}
}

// TestResolveProxyRefusals walks the sessions that must die before any
// upstream dial: each gets the error frame, the 4503 close naming the pool,
// and a fake upstream that recorded zero connections.
func TestResolveProxyRefusals(t *testing.T) {
	cases := []struct {
		name    string
		build   func(t *testing.T) *Handler
		wantMsg string // substring of the error frame's message
	}{
		{
			name: "assigned pool missing refuses session",
			build: func(t *testing.T) *Handler {
				repo := newTestRepo(t)
				addConnection(t, repo, "conn-1", "deepgram",
					`{"apiKey":"sk-test","proxyPoolId":"ghost-pool"}`)
				return NewHandler(repo, &chat.ChatHandler{Repo: repo, Client: &http.Client{}})
			},
			wantMsg: `proxy pool "ghost-pool" is assigned but does not exist`,
		},
		{
			name: "assigned pool inactive refuses session",
			build: func(t *testing.T) *Handler {
				repo := newTestRepo(t)
				poolID := seedPool(t, repo, db.ProxyPoolData{
					Name: "sleepy", ProxyURL: proxyTestProxyURL, Type: "http",
				})
				setPoolActive(t, repo, poolID, false)
				addConnection(t, repo, "conn-1", "deepgram",
					`{"apiKey":"sk-test","proxyPoolId":"`+poolID+`"}`)
				return NewHandler(repo, &chat.ChatHandler{Repo: repo, Client: &http.Client{}})
			},
			wantMsg: "is assigned but inactive",
		},
		{
			name: "strict edge-relay pool refuses session",
			build: func(t *testing.T) *Handler {
				repo := newTestRepo(t)
				poolID := seedPool(t, repo, db.ProxyPoolData{
					Name:        "relay",
					ProxyURL:    "https://strict-relay.example.vercel.app",
					Type:        "vercel",
					StrictProxy: true,
				})
				addConnection(t, repo, "conn-1", "deepgram",
					`{"apiKey":"sk-test","proxyPoolId":"`+poolID+`"}`)
				return NewHandler(repo, &chat.ChatHandler{Repo: repo, Client: &http.Client{}})
			},
			wantMsg: "edge relay and cannot tunnel",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			h := tt.build(t)
			base := serveHandler(t, h)
			f := newFakeUpstream(t, idleScript)
			client := dialClient(t, base, "model=dg/nova-3")

			frame := expectFrameType(t, client, "error")
			if msg, _ := frame["error"].(string); !strings.Contains(msg, tt.wantMsg) {
				t.Errorf("error frame = %q, want substring %q", msg, tt.wantMsg)
			}
			expectCloseError(t, client, 4503)

			// The refusal happens in resolveConnection, before dispatch —
			// the fake upstream can never have been upgraded.
			if f.upgradeQuery() != nil {
				t.Error("upstream was dialed; session must refuse before any dial")
			}
		})
	}
}

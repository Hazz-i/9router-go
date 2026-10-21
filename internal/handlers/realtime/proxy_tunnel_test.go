package realtime

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"9router/proxy/internal/db"
	"9router/proxy/internal/handlers/chat"
	"9router/proxy/internal/handlers/shared"

	"github.com/gorilla/websocket"
)

// startConnectProxy runs a minimal HTTP CONNECT proxy and reports the tunnel
// URL plus a counter of the CONNECT requests it served.
func startConnectProxy(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	var connects atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
			return
		}
		dest, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			dest.Close()
			http.Error(w, "hijack unsupported", http.StatusInternalServerError)
			return
		}
		client, _, err := hijacker.Hijack()
		if err != nil {
			dest.Close()
			return
		}
		connects.Add(1)
		if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
			client.Close()
			dest.Close()
			return
		}
		go func() {
			_, _ = io.Copy(dest, client)
			dest.Close()
		}()
		_, _ = io.Copy(client, dest)
		client.Close()
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &connects
}

// TestDialerForConnection_TunnelsThroughProxy proves the realtime lane's dialer
// actually opens the upstream WebSocket through the connection's HTTP proxy: a
// minimal CONNECT proxy sits in front of the fake upstream, and the echo only
// works if the tunnel carried the handshake.
func TestDialerForConnection_TunnelsThroughProxy(t *testing.T) {
	_, repo := newTestRepo(t)
	h := NewHandler(repo, &chat.ChatHandler{Repo: repo, Client: &http.Client{}})

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		hits.Add(1)
		mt, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		_ = conn.WriteMessage(mt, payload)
	}))
	defer upstream.Close()

	proxyURL, connects := startConnectProxy(t)
	pool, err := repo.InsertProxyPool(db.ProxyPoolData{
		Name: "local-connect", ProxyURL: proxyURL, Type: "http",
	})
	if err != nil {
		t.Fatalf("insert pool: %v", err)
	}

	dialer, err := h.dialerFor(&shared.ConnectionData{ProxyPoolID: pool["id"].(string)})
	if err != nil {
		t.Fatalf("dialerFor: %v", err)
	}

	wsURL := "ws" + strings.TrimPrefix(upstream.URL, "http")
	conn, _, err := dialer.DialContext(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("dial through proxy: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, reply, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(reply) != "ping" {
		t.Errorf("echo = %q, want %q", reply, "ping")
	}
	if got := connects.Load(); got != 1 {
		t.Errorf("proxy saw %d CONNECT requests, want 1", got)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("upstream saw %d upgrades, want 1", got)
	}
}

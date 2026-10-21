package chat

import (
	"fmt"
	"net/http"
	"net/url"
	"time"

	"9router/proxy/internal/log"
	internalproxy "9router/proxy/internal/proxy"

	"github.com/gorilla/websocket"
)

// wsHandshakeTimeout is the upstream handshake budget the caption lane already
// used; the realtime lane had none and now shares this one.
const wsHandshakeTimeout = 10 * time.Second

// WebSocketDialerForConnection returns a gorilla dialer that leaves through the
// same proxy as the connection's HTTP client: the assigned pool, then the legacy
// per-connection proxy, with the same strict refusal.
//
// gorilla only tunnels through an HTTP CONNECT proxy. An edge-relay pool
// (vercel/cloudflare/deno/netlify) forwards HTTP by rewriting the request, so it
// can never carry a socket: a strict relay refuses the session, a lax one dials
// direct with one warning, exactly as the HTTP path treats a relay it cannot
// tunnel through.
func (h *ChatHandler) WebSocketDialerForConnection(connData *ConnectionData) (*websocket.Dialer, error) {
	route, err := h.resolveProxyRoute(connData)
	if err != nil {
		return nil, err
	}
	if route.relay {
		if route.strict {
			log.Error("proxy", "strict edge relay pool cannot tunnel a websocket", "pool", route.poolID)
			return nil, fmt.Errorf("%w: proxy pool %q is an edge relay and cannot tunnel a WebSocket", internalproxy.ErrStrictProxyRequired, route.poolID)
		}
		log.Warn("proxy", "edge relay pool cannot tunnel a websocket, dialing direct", "pool", route.poolID, "url", route.url)
		// The relay endpoint is an HTTPS forwarder, not a CONNECT proxy: dialing
		// it as one answers "malformed HTTP status code". Same decision the HTTP
		// path makes, which routes the request through the relay by rewriting it.
		route.url = ""
	}

	// gorilla registers a dialer for the "http" scheme only, so an https://
	// proxy would fail inside the handshake with "unknown dialer type". Name it
	// here instead of letting the client read that.
	if route.url != "" {
		if u, err := url.Parse(route.url); err == nil && u.Scheme != "http" {
			return nil, fmt.Errorf("proxy pool %q uses the %q scheme; a WebSocket can only tunnel through an http:// proxy", route.poolID, u.Scheme)
		}
	}

	// The transport built for this route carries the proxy, the SSRF-safe
	// dialer that exempts the proxy endpoint, and the TLS config. Lifting those
	// three onto the gorilla dialer is what makes the tunnel identical to the
	// one the HTTP client opens.
	transport := internalproxy.NewSafeTransportForProxy(route.url)
	d := &websocket.Dialer{
		HandshakeTimeout: wsHandshakeTimeout,
		NetDialContext:   transport.DialContext,
		TLSClientConfig:  transport.TLSClientConfig,
	}
	d.Proxy = transport.Proxy
	if d.Proxy == nil {
		// The HTTP client leaves Proxy nil and the stdlib applies HTTP_PROXY;
		// gorilla has to be told explicitly or it always dials direct.
		d.Proxy = http.ProxyFromEnvironment
	}
	return d, nil
}

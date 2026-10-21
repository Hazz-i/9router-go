### Realtime Voice and Live Caption now dial their upstream through the connection's proxy

Both WebSocket lanes dialed from the server's own address regardless of how the
connection was configured: `realtimeDialer` and `caption/handler.go`'s `dial`
were bare `websocket.Dialer{}` values, and the shortcut comments said so
(`realtime/handler.go`, `caption/session.go`: "no proxy-pool dialing"). A
provider connection assigned to a proxy pool kept leaking the operator's egress
IP on the socket lanes while the HTTP lanes were proxied, and the dashboard kept
showing the connection as proxied.

- `chat.ChatHandler` gains `WebSocketDialerForConnection`, and the proxy decision
  it shares with the HTTP path is now one function (`resolveProxyRoute`): the
  assigned pool, then the legacy per-connection proxy, then the strict rule. The
  HTTP client builder reads the same struct, so the two transports cannot
  disagree about which pool a connection uses.
- Both lanes resolve that dialer per session, before any upstream socket opens.
  gorilla tunnels through an HTTP CONNECT proxy and authenticates from the pool
  URL's userinfo; the dialer lifts `Proxy`, the SSRF-safe `DialContext` that
  exempts the proxy endpoint, and the TLS config off the transport the HTTP path
  would have used, so the tunnel is the same one.
- An edge-relay pool (vercel/cloudflare/deno/netlify) forwards HTTP by rewriting
  the request and can never carry a socket: a strict relay refuses the session
  with `ErrStrictProxyRequired` (realtime: `proxy_error` + close 4500, caption:
  4503 error frame) instead of dialing direct, and a lax relay dials direct with
  one warning, the same choice the HTTP path makes. An `https://` pool URL is
  named in the error rather than left to gorilla's "unknown dialer type".
- `realtime.NewHandler` / `caption.NewHandler` take the chat handler; `nil` keeps
  the direct dialer, which is what the unit fixtures and any direct-only setup
  use. The remaining `shortcut:` comments now state only the limit that is still
  true (no mid-session account rotation for a socket session).

Verified with `go test -count=1 ./internal/handlers/chat/
./internal/handlers/realtime/ ./internal/handlers/caption/`: table coverage for
every route (pool, legacy proxy, strict relay refused, lax relay direct, https
scheme, missing pool) plus `TestDialerForConnection_TunnelsThroughProxy`, which
puts a real HTTP CONNECT proxy in front of a fake upstream and proves the
handshake and echo travel through it, and the caption refusals which assert the
fake upstream recorded zero upgrades.

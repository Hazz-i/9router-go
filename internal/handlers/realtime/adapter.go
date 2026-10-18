package realtime

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"9router/proxy/internal/handlers/shared"
	"9router/proxy/internal/providers"
	"github.com/gorilla/websocket"
)

// closeWriteTimeout bounds the courtesy close-frame write to the client and
// upstream sockets.
const closeWriteTimeout = time.Second

// Adapter is one provider's realtime-voice upstream connection. It mirrors the
// upstream seam (src/sse/handlers/realtime/index.js): construction resolves the
// adapter's own configuration and may fail synchronously (surfaced as
// realtime_config_error + close 4400); Connect dials the upstream and reports
// socket failures asynchronously (server_error + close 4500). Adapters emit to
// the client ONLY through the events.go builders and the adapterCtx senders.
type Adapter interface {
	// Connect dials the upstream and starts its read pump. It returns once the
	// dial is established (or failed); the session closes the client on error.
	Connect() error
	// HandleClientEvent handles one parsed client JSON event (OpenAI Realtime
	// protocol). Called only from the client read loop.
	HandleClientEvent(event map[string]any)
	// HandleClientAudio forwards one raw client PCM16 frame upstream.
	HandleClientAudio(data []byte)
	// Close clears timers and closes the upstream with code 1000
	// "Client closed". Called exactly once, from teardown.
	Close()
}

// adapterFactory builds one adapter against the shared session context. Like
// upstream's REALTIME_ADAPTERS, a new provider is added by data alone.
type adapterFactory func(*adapterCtx) (Adapter, error)

// realtimeAdapters is the Go port of upstream's REALTIME_ADAPTERS registry,
// keyed by canonical provider id.
var realtimeAdapters = map[string]adapterFactory{
	"gemini":   newGeminiAdapter,
	"deepgram": newDeepgramAdapter,
}

// adapterCtx is the shared context upstream JS hands every adapter: provider
// config, resolved credentials, the client senders and the usage counters.
type adapterCtx struct {
	cfg          *providers.RealtimeConfig
	provider     string
	model        string
	voice        string
	instructions string
	apiKey       string
	connData     *shared.ConnectionData
	query        url.Values
	sess         *session
}

// overrideURL returns the per-connection baseUrl override (providerSpecificData
// first, matching where the newer dashboard writer stores it, then the legacy
// top-level key), or "" when the connection has none.
func (c *adapterCtx) overrideURL() string {
	if c.connData == nil {
		return ""
	}
	if c.connData.ProviderSpecificData != nil {
		if u, _ := c.connData.ProviderSpecificData["baseUrl"].(string); u != "" {
			return u
		}
	}
	return c.connData.BaseURL
}

func (c *adapterCtx) sendClient(v any)               { c.sess.sendClient(v) }
func (c *adapterCtx) closeClient(code int, r string) { c.sess.closeClient(code, r) }
func (c *adapterCtx) isClientOpen() bool             { return !c.sess.isClosed() }
func (c *adapterCtx) countInput(n int)               { c.sess.countInput(n) }
func (c *adapterCtx) countOutput(n int)              { c.sess.countOutput(n) }
func (c *adapterCtx) setOutputRate(r int)            { c.sess.setOutputRate(r) }
func (c *adapterCtx) millisSinceStart() int64 {
	return time.Since(c.sess.start).Milliseconds()
}

// upstreamSocket wraps the upstream connection so the heartbeat/keepalive
// goroutine, the adapter's upstream read pump and the client read loop can
// share one socket: gorilla/websocket allows exactly one concurrent writer.
type upstreamSocket struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func (u *upstreamSocket) sendJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal upstream frame: %w", err)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.conn.WriteMessage(websocket.TextMessage, b)
}

func (u *upstreamSocket) sendBinary(b []byte) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.conn.WriteMessage(websocket.BinaryMessage, b)
}

func (u *upstreamSocket) isOpen() bool { return u != nil && u.conn != nil }

func (u *upstreamSocket) close(code int, reason string) {
	if u == nil || u.conn == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	_ = u.conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(code, reason), time.Now().Add(closeWriteTimeout))
	_ = u.conn.Close()
}

// toWSDialURL normalizes an http(s) URL into a ws(s) dial URL — gorilla only
// accepts ws/wss schemes, while a per-connection baseUrl is stored as http(s).
func toWSDialURL(raw string) string {
	switch {
	case strings.HasPrefix(raw, "https://"):
		return "wss://" + raw[len("https://"):]
	case strings.HasPrefix(raw, "http://"):
		return "ws://" + raw[len("http://"):]
	default:
		return raw
	}
}

// newID mints upstream's `sess_`/`resp_`/`item_` identifiers: prefix plus
// 12 random bytes hex.
func newID(prefix string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return prefix + "000000000000000000000000"
	}
	return prefix + hex.EncodeToString(b)
}

func orDefaultInt(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

func orDefaultStr(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

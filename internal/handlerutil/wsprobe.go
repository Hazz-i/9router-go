package handlerutil

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// wsProbeTimeout mirrors upstream's AbortSignal.timeout(15000) for one probe.
const wsProbeTimeout = 15 * time.Second

// wsProbeDialer dials the gateway's own WebSocket endpoint. Package-level so
// tests can point the probe at a fake WS server.
var wsProbeDialer = func(ctx context.Context, rawURL string) (*websocket.Conn, error) {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, rawURL, nil)
	return conn, err
}

// WSProbeResult is the dashboard verdict for one WebSocket model probe. The
// JSON shape is exactly the per-model Test chip's answer ({ok, latencyMs,
// error}), so the SPA renders a WS probe like every other kind with no second
// code path.
type WSProbeResult struct {
	OK        bool   `json:"ok"`
	LatencyMs int64  `json:"latencyMs"`
	Error     string `json:"error,omitempty"`
}

// ProbeWebSocketModel ports upstream ping.js probeLocalWs + the
// realtime/liveCaption branch of pingModelByKind (ping.js:58-106, :218-230):
// the dashboard pings the gateway's own WebSocket endpoint — the same sockets
// the features serve — because POSTing these kinds at the chat path would probe
// the wrong transport entirely.
//
//   - kind "realtime"     → GET ws://<host>/v1/realtime?model=<model>&voice=Puck, success on the first `session.created` frame
//   - kind "liveCaption"  → GET ws://<host>/v1/audio/live-transcriptions?model=<model>&language=id&encoding=linear16&sample_rate=16000, success on the first `ready` frame
//
// An `error` frame fails with its message, a close before the success frame
// fails with the close code, and the whole probe is bounded by the 15 s
// connection probe timeout. The socket is always closed.
//
// gatewayURL is the gateway's own http(s) base (scheme is rewritten to
// ws(s)); the caller passes the incoming request's host, which is where the
// browser reached the dashboard.
//
// It lives here rather than in dashboard because the media model-test dispatch
// is its only caller, and dashboard imports media (uc-web cookie validation):
// a dashboard-owned probe would close that into an import cycle.
func ProbeWebSocketModel(ctx context.Context, gatewayURL, model, kind, apiKey string) WSProbeResult {
	start := time.Now()
	latency := func() int64 { return time.Since(start).Milliseconds() }

	okType, rawURL, err := wsProbeURL(gatewayURL, model, kind, apiKey)
	if err != nil {
		return WSProbeResult{Error: err.Error()}
	}

	ctx, cancel := context.WithTimeout(ctx, wsProbeTimeout)
	defer cancel()

	conn, dialErr := wsProbeDialer(ctx, rawURL)
	if dialErr != nil {
		return WSProbeResult{LatencyMs: latency(), Error: TruncateProbeMessage(dialErr.Error())}
	}
	defer func() { _ = conn.Close() }()

	// Read text frames until the success frame, an error frame, or the
	// timeout. One deadline covers both the dial and every frame read.
	_ = conn.SetReadDeadline(DeadlineFrom(ctx))
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			// gorilla surfaces a close handshake the server initiated through
			// the CloseError's code; anything else is a timeout or transport
			// drop and carries its own message.
			var closeErr *websocket.CloseError
			if errors.As(err, &closeErr) {
				return WSProbeResult{LatencyMs: latency(), Error: TruncateProbeMessage(fmt.Sprintf("WebSocket closed (code %d) before %s", closeErr.Code, okType))}
			}
			if ctx.Err() != nil {
				return WSProbeResult{LatencyMs: latency(), Error: fmt.Sprintf("Timed out waiting for WS %s after %.0fs", okType, wsProbeTimeout.Seconds())}
			}
			return WSProbeResult{LatencyMs: latency(), Error: TruncateProbeMessage(err.Error())}
		}

		frameType, detail, isErr := parseWSProbeFrame(msg)
		if frameType == okType {
			return WSProbeResult{OK: true, LatencyMs: latency()}
		}
		if isErr {
			message := fmt.Sprintf("WS gateway returned an error before %s", okType)
			if detail != "" {
				message = detail
			}
			return WSProbeResult{LatencyMs: latency(), Error: TruncateProbeMessage(message)}
		}
	}
}

// wsProbeURL builds the gateway WS URL for one kind, mirroring ping.js:218-230
// byte for byte in path and query shape.
func wsProbeURL(gatewayURL, model, kind, apiKey string) (okType string, rawURL string, err error) {
	base, err := url.Parse(strings.TrimSpace(gatewayURL))
	if err != nil {
		return "", "", fmt.Errorf("invalid gateway URL: %w", err)
	}
	switch base.Scheme {
	case "https":
		base.Scheme = "wss"
	case "http", "ws", "wss", "":
		base.Scheme = "ws"
	default:
		return "", "", fmt.Errorf("unsupported gateway scheme %q", base.Scheme)
	}

	query := base.Query()
	switch kind {
	case "realtime":
		okType = "session.created"
		base.Path = "/v1/realtime"
		query.Set("model", model)
		query.Set("voice", "Puck")
	case "liveCaption":
		okType = "ready"
		base.Path = "/v1/audio/live-transcriptions"
		query.Set("model", model)
		query.Set("language", "id")
		query.Set("encoding", "linear16")
		query.Set("sample_rate", "16000")
	default:
		return "", "", fmt.Errorf("unsupported model kind: %s", kind)
	}
	if apiKey != "" {
		// Same gateways the features use; query-param key per extractTokenFromRequest.
		query.Set("apiKey", apiKey)
	}
	base.RawQuery = query.Encode()
	return okType, base.String(), nil
}

// parseWSProbeFrame reads the frame's `type`, and for `error` frames the most
// specific detail: error.message, then error, then message (ping.js:92-99).
func parseWSProbeFrame(msg []byte) (frameType, detail string, isErr bool) {
	var frame struct {
		Type    string `json:"type"`
		Error   any    `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(msg, &frame) != nil {
		return "", "", false
	}
	if frame.Type != "error" {
		return frame.Type, "", false
	}
	switch v := frame.Error.(type) {
	case map[string]any:
		if m, ok := v["message"].(string); ok && m != "" {
			return "error", m, true
		}
	case string:
		if v != "" {
			return "error", v, true
		}
	}
	return "error", frame.Message, true
}

// DeadlineFrom is the context deadline as a wall-clock read deadline, or a
// near-term deadline when the context carries none.
func DeadlineFrom(ctx context.Context) time.Time {
	if d, ok := ctx.Deadline(); ok {
		return d
	}
	return time.Now().Add(wsProbeTimeout)
}

// TruncateProbeMessage mirrors ping.js's String(detail).slice(0, 240).
func TruncateProbeMessage(s string) string {
	if len(s) <= 240 {
		return s
	}
	return s[:240]
}

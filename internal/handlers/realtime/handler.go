package realtime

import (
	json "encoding/json/v2"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"9router/proxy/internal/db"
	"9router/proxy/internal/handlers/chat"
	"9router/proxy/internal/handlers/shared"
	"9router/proxy/internal/log"
	"9router/proxy/internal/models"
	"9router/proxy/internal/providers"
	"github.com/gorilla/websocket"
)

// defaultRealtimeModel is the `model` query-param default, and the provider
// a slash-less model string falls back to (upstream's getModelInfo default).
const defaultRealtimeModel = "gemini/gemini-3.8-live"

// defaultInstructions is upstream's hardcoded session instruction line
// (realtimeVoiceWs.js ctx.instructions).
const defaultInstructions = "You are a helpful and concise AI voice assistant. Speak naturally in Indonesian or English."

// gatewayUpgrader accepts every origin: the dashboard and third-party browser
// clients upgrade cross-origin, matching upstream's WebSocketServer (no origin
// check).
var gatewayUpgrader = websocket.Upgrader{
	CheckOrigin: func(*http.Request) bool { return true },
}

// Handler serves GET/POST /v1/realtime (and the /api/v1 alias routed by the
// router lane). It owns auth, provider/credential resolution, the client
// socket plumbing and session usage accounting; upstream translation lives in
// the per-provider adapters.
type Handler struct {
	repo *db.Repo
	// configFor is the provider→RealtimeConfig seam. It exists so tests can
	// inject fixture configs; production always uses providers.GetRealtimeConfig.
	configFor func(string) *providers.RealtimeConfig
	// dialFor maps a connection to the WebSocket dialer that leaves through
	// the connection's own proxy chain. It is the chat handler's
	// WebSocketDialerForConnection in production; nil (tests, direct
	// construction) keeps sessions dialing direct.
	dialFor func(*shared.ConnectionData) (*websocket.Dialer, error)
	// wg counts live sessions. waitSessions drains it so test teardown (and
	// any future shutdown path) never closes the repo under a session.
	wg sync.WaitGroup
}

// waitSessions blocks until every session started through this handler has
// fully torn down (usage row written, goroutines joined).
func (h *Handler) waitSessions() { h.wg.Wait() }

func NewHandler(repo *db.Repo, chatH *chat.ChatHandler) *Handler {
	h := &Handler{repo: repo, configFor: providers.GetRealtimeConfig}
	if chatH != nil {
		h.dialFor = chatH.WebSocketDialerForConnection
	}
	return h
}

// dialerFor resolves the outbound dialer for one connection: the connection's
// proxy chain when one is wired, else the shared direct dialer.
func (h *Handler) dialerFor(connData *shared.ConnectionData) (*websocket.Dialer, error) {
	if h.dialFor == nil {
		return &realtimeDialer, nil
	}
	return h.dialFor(connData)
}

// HandleWS serves one request on /v1/realtime. Without an Upgrade header it
// returns the JSON protocol-info document; with one it upgrades and runs the
// whole realtime-voice session until the client disconnects.
func (h *Handler) HandleWS(w http.ResponseWriter, r *http.Request) {
	if !websocket.IsWebSocketUpgrade(r) {
		// Non-upgrade requests follow the upstream route semantics: GET (and
		// anything not POST) gets the info document, POST gets 426.
		if r.Method == http.MethodPost {
			h.HandleUpgradeRequired(w, r)
			return
		}
		h.serveInfo(w)
		return
	}
	conn, err := gatewayUpgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote the HTTP error response.
		return
	}
	defer func() {
		if err := conn.Close(); err != nil {
			log.Warn("REALTIME_VOICE", "client websocket close failed", "error", err)
		}
	}()
	h.wg.Add(1)
	defer h.wg.Done()
	h.serveSession(conn, r)
}

// serveInfo is the GET document from upstream's route.js: a description of the
// OpenAI-compatible realtime protocol the gateway speaks.
func (h *Handler) serveInfo(w http.ResponseWriter) {
	body := map[string]any{
		"status":          "ready",
		"protocol":        "websocket",
		"message":         "Connect to this endpoint using WebSocket (ws:// or wss://) for bidirectional realtime voice conversation.",
		"endpoint":        "/v1/realtime",
		"protocolFormat":  "OpenAI Realtime API compatible",
		"backend":         "Google Gemini Multimodal Live API",
		"defaultModel":    "gemini-3.8-live",
		"supportedVoices": []string{"Puck", "Charon", "Kore", "Fenrir", "Aoede"},
		"queryParameters": map[string]any{
			"model":  "Model ID (default: gemini/gemini-3.8-live)",
			"voice":  "Voice name (default: Puck)",
			"apiKey": "NineRouter API Key (if required by settings)",
		},
		"events": map[string]any{
			"client_to_server": []string{
				"{ type: 'session.update', session: { voice, instructions } }",
				"{ type: 'input_audio_buffer.append', audio: '<base64 PCM16>' }",
				"{ type: 'input_audio_buffer.clear' }",
				"{ type: 'response.create' }",
				"{ type: 'conversation.item.create', item: { content: [...] } }",
				"Binary WebSocket frames (PCM 16-bit 16kHz audio)",
			},
			"server_to_client": []string{
				"{ type: 'session.created', session: { ... } }",
				"{ type: 'session.updated', session: { ... } }",
				"{ type: 'response.audio.delta', delta: '<base64 PCM24k>' }",
				"{ type: 'response.audio_transcript.delta', delta: '...' }",
				"{ type: 'input_audio_buffer.speech_started' }",
				"{ type: 'response.cancelled' }",
				"{ type: 'response.audio.done' }",
				"{ type: 'response.done' }",
				"{ type: 'error', error: { ... } }",
			},
		},
	}
	b, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// HandleUpgradeRequired serves POST on the realtime paths: upstream answers
// with 426 and the upstream error body.
func (h *Handler) HandleUpgradeRequired(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Upgrade", "WebSocket")
	w.Header().Set("Connection", "Upgrade")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusUpgradeRequired)
	body := map[string]string{
		"error": fmt.Sprintf("Upgrade Required: %s requires a WebSocket connection (ws:// or wss://)", r.URL.Path),
	}
	b, _ := json.Marshal(body)
	_, _ = w.Write(b)
}

// serveSession runs one upgraded realtime-voice session. Every failure path
// sends the OpenAI `error` event before its close frame, in upstream's order.
func (h *Handler) serveSession(conn *websocket.Conn, r *http.Request) {
	q := r.URL.Query()

	// 1. Auth gate (only when settings.requireApiKey) — browsers cannot set
	// headers on a WebSocket, so the token order starts at the query string.
	if h.requireAPIKey() && !h.authenticated(r, q) {
		sendPreSessionError(conn, "invalid_request_error", "unauthorized", "Unauthorized: Invalid or missing API key")
		closePreSession(conn, 4401, "Unauthorized")
		return
	}

	// 2. Model → provider → realtimeConfig resolution.
	cfg, provider, targetModel := h.resolveModel(q)
	if cfg == nil {
		sendPreSessionError(conn, "invalid_request_error", "provider_unsupported",
			fmt.Sprintf("Provider %s does not support realtime voice", providerToken(q)))
		closePreSession(conn, 4400, "Unsupported provider")
		return
	}

	// 3. Provider credentials: dashboard connection first, then <PROVIDER>_API_KEY.
	connRow, connData := h.findConnection(provider)
	apiKey := credentialAPIKey(connData, provider)
	if apiKey == "" && provider == "gemini" {
		// Gemini-only second chance: any gemini account in the db, then the env.
		connRow, connData = h.findConnection("gemini")
		apiKey = credentialAPIKey(connData, "gemini")
	}
	if apiKey == "" {
		sendPreSessionError(conn, "invalid_request_error", "provider_missing_credentials",
			fmt.Sprintf("No API key configured for %s. Please add a %s connection in 9Router Dashboard or set %s_API_KEY environment variable.",
				provider, provider, strings.ToUpper(provider)))
		closePreSession(conn, 4401, "Missing provider credentials")
		return
	}

	// 4. Outbound proxy: the upstream dialer honors the connection's proxy
	// pool, the same chain the HTTP path uses. A strict pool the socket
	// cannot tunnel through refuses the session before anything opens.
	dial, err := h.dialerFor(connData)
	if err != nil {
		sendPreSessionError(conn, "server_error", "proxy_error", fmt.Sprintf("Proxy error: %v", err))
		closePreSession(conn, 4500, "Proxy error")
		return
	}

	// 5. Adapter construction — Deepgram's ?listen/?think/?speak resolution
	// throws here, before any upstream socket is opened.
	s := newSession(h.repo, conn)
	s.provider = provider
	s.model = targetModel
	if connRow != nil {
		s.connID = connRow.ID
		s.connAPIKey = connData.APIKey
	}
	c := &adapterCtx{
		cfg:          cfg,
		provider:     provider,
		model:        targetModel,
		voice:        queryDefault(q, "voice", "Puck"),
		instructions: defaultInstructions,
		apiKey:       apiKey,
		connData:     connData,
		query:        q,
		sess:         s,
		dial:         dial,
	}
	ad, err := createRealtimeAdapter(provider, c)
	if err != nil {
		sendPreSessionError(conn, "invalid_request_error", "realtime_config_error", err.Error())
		closePreSession(conn, 4400, truncateReason(err.Error()))
		return
	}

	// 6. Session: the client read pump owns this goroutine; the upstream dial
	// is async exactly like upstream's adapter.connect() promise. The dial
	// goroutine is tracked by the session's WaitGroup so teardown joins it
	// even while it is still blocked in DialContext (ctx cancel aborts that).
	defer s.teardown(ad)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := ad.Connect(); err != nil {
			log.Error("REALTIME_VOICE", "failed to initiate upstream connection", "error", err)
			c.sendClient(map[string]any{
				"type": "error",
				"error": map[string]any{
					"type":    "server_error",
					"message": fmt.Sprintf("Failed to initiate upstream connection: %v", err),
				},
			})
			c.closeClient(4500, "Upstream error")
		}
	}()
	s.readLoop(ad)
}

// createRealtimeAdapter is the port of upstream's createRealtimeAdapter: the
// provider id selects the factory; unknown providers throw before any dial.
func createRealtimeAdapter(providerID string, c *adapterCtx) (Adapter, error) {
	factory, ok := realtimeAdapters[providerID]
	if !ok {
		// Client-visible: ported verbatim from upstream (error frame message + 4400 close reason).
		return nil, fmt.Errorf("Provider %q does not support realtime voice", providerID) //nolint:staticcheck // this text is the frame payload a client receives; it is ported verbatim from upstream.
	}
	return factory(c)
}

func (h *Handler) requireAPIKey() bool {
	raw, err := h.repo.GetSettingsRaw()
	if err != nil || raw == nil {
		return false
	}
	v, _ := raw["requireApiKey"].(bool)
	return v
}

// authenticated reproduces upstream's extractTokenFromRequest order: query
// params apiKey/api_key/token/key, then Authorization Bearer, then x-api-key.
func (h *Handler) authenticated(r *http.Request, q url.Values) bool {
	token := extractToken(r, q)
	if token == "" {
		return false
	}
	valid, err := h.repo.ValidateApiKey(token)
	if err != nil {
		log.Error("REALTIME_VOICE", "api key validation failed", "error", err)
		return false
	}
	return valid
}

func extractToken(r *http.Request, q url.Values) string {
	for _, k := range []string{"apiKey", "api_key", "token", "key"} {
		if v := q.Get(k); v != "" {
			return v
		}
	}
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimSpace(auth[len("Bearer "):])
	}
	if x := r.Header.Get("X-Api-Key"); x != "" {
		return strings.TrimSpace(x)
	}
	return ""
}

// resolveModel maps the `model` query param onto provider + target model.
// A slash-less value is routed to the default provider (gemini) with the whole
// string as the model; alias resolution happens inside GetRealtimeConfig.
func (h *Handler) resolveModel(q url.Values) (*providers.RealtimeConfig, string, string) {
	raw := q.Get("model")
	if raw == "" {
		raw = defaultRealtimeModel
	}
	providerToken, modelPart, hasSlash := strings.Cut(raw, "/")
	if !hasSlash {
		providerToken, modelPart = "gemini", raw
	}
	cfg := h.configFor(providerToken)
	if cfg == nil {
		return nil, "", ""
	}
	if modelPart == "" {
		modelPart = cfg.DefaultModel
	}
	return cfg, providerToken, modelPart
}

func providerToken(q url.Values) string {
	raw := q.Get("model")
	if raw == "" {
		raw = defaultRealtimeModel
	}
	providerToken, _, _ := strings.Cut(raw, "/")
	return providerToken
}

// findConnection picks the first active connection of the provider that
// carries an API key.
//
// shortcut: no mid-session account rotation for a socket session, upgrade
// when a WS-through-proxy provider appears.
func (h *Handler) findConnection(provider string) (*models.ProviderConnection, *shared.ConnectionData) {
	conns, err := h.repo.GetProviderConnections(provider, true)
	if err != nil {
		log.Warn("REALTIME_VOICE", "connection lookup failed", "provider", provider, "error", err)
		return nil, nil
	}
	for _, conn := range conns {
		cd := parseConnectionData(conn)
		if cd.APIKey != "" {
			return conn, cd
		}
	}
	return nil, nil
}

func parseConnectionData(conn *models.ProviderConnection) *shared.ConnectionData {
	var cd shared.ConnectionData
	if conn != nil {
		_ = json.Unmarshal([]byte(conn.Data), &cd)
	}
	return &cd
}

// credentialAPIKey reads the connection key, falling back to the provider env
// var — mirroring getProviderCredentials.
func credentialAPIKey(connData *shared.ConnectionData, provider string) string {
	if connData != nil && connData.APIKey != "" {
		return connData.APIKey
	}
	return os.Getenv(strings.ToUpper(provider) + "_API_KEY")
}

func queryDefault(q url.Values, key, def string) string {
	if v := q.Get(key); v != "" {
		return v
	}
	return def
}

func truncateReason(msg string) string {
	r := []rune(msg)
	if len(r) > 100 {
		return string(r[:100])
	}
	return msg
}

// sendPreSessionError writes the OpenAI error event before any session state
// exists (plain conn writers, one goroutine).
func sendPreSessionError(conn *websocket.Conn, errType, code, message string) {
	errBody := map[string]any{"type": errType, "message": message}
	if code != "" {
		errBody["code"] = code
	}
	_ = conn.WriteMessage(websocket.TextMessage, marshalEvent(map[string]any{
		"type":  "error",
		"error": errBody,
	}))
}

func closePreSession(conn *websocket.Conn, code int, reason string) {
	closeConn(conn, code, reason)
}

// closeConn sends the close frame (with reason) then closes the socket.
func closeConn(conn *websocket.Conn, code int, reason string) {
	_ = conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(code, reason), time.Now().Add(closeWriteTimeout))
	_ = conn.Close()
}

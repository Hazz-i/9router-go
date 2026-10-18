// Package caption serves the live-caption WebSocket session on
// /v1/audio/live-transcriptions (Go port of upstream 9router
// src/sse/handlers/liveCaptionWs.js).
//
// A session proxies a browser WebSocket to one STT upstream (Deepgram,
// ElevenLabs Scribe, Gemini Live, or a selfhosted OpenAI-compatible endpoint),
// turning interim/final transcripts into committed word events, subtitle
// chunks, and on-demand SRT/ASS exports. Usage is recorded once per session at
// cleanup with one token per committed word.
package caption

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"9router/proxy/internal/db"
	"9router/proxy/internal/handlerutil"
	"9router/proxy/internal/middleware"
	"9router/proxy/internal/providers"

	"github.com/gorilla/websocket"
)

const captionEndpoint = "/v1/audio/live-transcriptions"

// Handler owns the caption endpoints. Everything below it is per-session
// state; the Handler carries only shared clients and the upstream endpoints
// the session dials.
type Handler struct {
	repo *db.Repo
	// supported is the provider capability gate (liveCaption or stt service
	// kinds). It is a field rather than a direct call so tests can stand in
	// for Lane A's catalog without weakening the production call site.
	supported func(provider string) bool

	// Upstream endpoints. Defaults come from the provider catalog
	// (providers.GetLiveCaptionConfig) where it carries them, else the URLs
	// upstream hardcodes in the handler itself. Tests point these at fake
	// upstreams.
	deepgramURL    string
	elevenlabsURL  string
	geminiURL      string
	selfhostedBase string

	upgrader websocket.Upgrader
	dial     websocket.Dialer
	http     *http.Client
}

// NewHandler builds the caption handler over the shared repo.
func NewHandler(repo *db.Repo) *Handler {
	h := &Handler{
		repo:      repo,
		supported: providers.SupportsLiveCaption,
		// Fallbacks are exactly the URLs upstream's handler hardcodes; the
		// catalog overrides them below when it carries a value.
		deepgramURL:    "wss://api.deepgram.com/v1/listen",
		elevenlabsURL:  "wss://api.elevenlabs.io/v1/speech-to-text/realtime",
		geminiURL:      "wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent",
		selfhostedBase: "http://localhost:8080/v1/audio/transcriptions",
		upgrader:       websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
		dial:           websocket.Dialer{HandshakeTimeout: 10 * time.Second},
		http:           &http.Client{},
	}
	if cfg := providers.GetLiveCaptionConfig("deepgram"); cfg != nil && cfg.WsURL != "" {
		h.deepgramURL = cfg.WsURL
	}
	if cfg := providers.GetLiveCaptionConfig("elevenlabs"); cfg != nil && cfg.WsURL != "" {
		h.elevenlabsURL = cfg.WsURL
	}
	if cfg := providers.GetLiveCaptionConfig("selfhosted-stt"); cfg != nil && cfg.BaseURL != "" {
		h.selfhostedBase = cfg.BaseURL
	}
	return h
}

// HandleWS serves GET on the caption paths: without an Upgrade header it is
// the REST protocol-info document; with one it upgrades and runs the session.
// Both /v1 and /api/v1 spellings serve identically.
func (h *Handler) HandleWS(w http.ResponseWriter, r *http.Request) {
	if !isWSUpgrade(r) {
		h.writeInfo(w)
		return
	}

	// The caller key must be read before the upgrade: hijacking cancels the
	// request context, and the session outlives it.
	clientKey := middleware.AuthenticatedApiKeyFromContext(r.Context())

	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade already wrote the handshake error
	}
	h.runSession(conn, urlValues(r.URL.Query()), clientKey)
}

// HandleUpgradeRequired serves POST on the caption paths: the endpoint speaks
// WebSocket only, so a plain POST is answered with 426 and the upgrade hint.
func (h *Handler) HandleUpgradeRequired(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Upgrade", "WebSocket")
	w.Header().Set("Connection", "Upgrade")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	handlerutil.WriteJSON(w, http.StatusUpgradeRequired, map[string]string{
		"error": fmt.Sprintf("Upgrade Required: %s requires a WebSocket connection (ws:// or wss://)", captionEndpoint),
	})
}

// isWSUpgrade reports whether the request carries the WebSocket upgrade
// headers (gorilla v1.5.3 has no IsUpgradeRequest).
func isWSUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

// writeInfo ports the REST info endpoint (src/app/api/v1/audio/
// live-transcriptions/route.js GET).
func (h *Handler) writeInfo(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
		"status":             "ready",
		"protocol":           "websocket",
		"message":            "Connect to this endpoint using WebSocket (ws:// or wss://) to stream live audio chunks.",
		"endpoint":           captionEndpoint,
		"supportedEncodings": []string{"linear16", "pcm", "webm_opus"},
		"queryParameters": map[string]string{
			"model":       "Model ID (e.g. dg/nova-3, gemini/gemini-2.5-flash-native-audio-preview-09-17)",
			"language":    "Language code (e.g. id, en)",
			"encoding":    "linear16 | webm (default: linear16)",
			"sample_rate": "Sample rate in Hz (default: 16000)",
			"apiKey":      "NineRouter API Key (if required by settings)",
		},
		"events": map[string]any{
			"client_to_server": []string{
				"Binary audio chunks (PCM 16-bit 16kHz or WebM Opus)",
				"{ type: 'keepalive' }",
				"{ type: 'finalize' }",
				"{ type: 'get_subtitles', format: 'srt' | 'ass' | 'all' }",
			},
			"server_to_client": []string{
				"{ type: 'connected', model, language, ... }",
				"{ type: 'ready', upstream }",
				"{ type: 'word', word, start, end, confidence, isFinal }",
				"{ type: 'chunk', chunk: { text, start, end, words } }",
				"{ type: 'subtitles', srt, ass, chunks }",
				"{ type: 'error', error }",
			},
		},
	})
}

// closeWith sends a WS close code after a terminal protocol failure. The
// upstream gateway sends an error message before every close, so callers
// write the frame first.
func closeWith(conn *websocket.Conn, code int, reason string) {
	_ = conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(code, reason), time.Now().Add(5*time.Second))
	_ = conn.Close()
}

// maskKey is the persistence-safe form of a key for the usage row.
func maskKey(key string) string { return handlerutil.MaskAPIKey(key) }

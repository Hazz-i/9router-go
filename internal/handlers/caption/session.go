// This file owns the per-session lifecycle: query parsing, connection
// resolution, the client read loop, dispatch, and cleanup + usage recording.
// The four upstream backends live in their own files (deepgram.go,
// elevenlabs.go, gemini.go, selfhosted.go).
package caption

import (
	"context"
	json "encoding/json/v2"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"9router/proxy/internal/handlers/shared"
	"9router/proxy/internal/handlerutil"
	"9router/proxy/internal/log"
	"9router/proxy/internal/models"
	"9router/proxy/internal/providers"

	"github.com/gorilla/websocket"
)

// session is one live-caption socket. It is created after the client upgrade
// and pinned to one connection (see the shortcut note in resolveConnection).
type session struct {
	h       *Handler
	client  *websocket.Conn
	clientK string // masked caller key, pre-captured for usage attribution

	ctx    context.Context
	cancel context.CancelFunc

	provider   string // canonical id
	model      string
	language   string
	encoding   string
	sampleRate int

	connID   string
	connData *shared.ConnectionData
	// dial is the per-connection proxy dialer resolved in resolveConnection.
	// nil keeps the handler's default dialer (direct, no proxy chain).
	dial *websocket.Dialer

	// word state is written by the upstream reader and read by the client
	// reader (get_subtitles) and by cleanup (usage count).
	wordsMu sync.Mutex
	words   []Word

	writeMu sync.Mutex // serializes gateway→client frames (two reader goroutines)

	// wg tracks every goroutine the session spawns (upstream read pumps).
	// cleanup joins them, so nothing outlives the repo the usage row is
	// written against or the client socket the handler closes on return.
	wg sync.WaitGroup

	// Backend seams, filled in by the dispatched setup function.
	forward   func([]byte) // client binary → upstream
	finalize  func()       // client {type:"finalize"}
	keepalive func()       // client {type:"keepalive"}

	upMu     sync.Mutex // guards up and upClosed
	up       *websocket.Conn
	upClosed bool
}

// runSession runs the whole life of one upgraded socket.
func (h *Handler) runSession(conn *websocket.Conn, q urlValues, clientKey *models.APIKey) {
	s := &session{h: h, client: conn}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	if clientKey != nil {
		s.clientK = maskKey(clientKey.Key)
	}
	defer func() { _ = s.client.Close() }()

	modelParam := q.get("model", "dg/nova-3")
	s.language = q.get("language", q.get("lang", "id"))
	s.encoding = q.get("encoding", "linear16")
	s.sampleRate = parseRate(q.get("sample_rate", q.get("sampleRate", "16000")))

	if !s.parseModel(modelParam) {
		return
	}
	if !h.supported(s.provider) {
		s.errAndClose(4400, "Unsupported provider",
			fmt.Sprintf("Provider %s does not support live caption / STT", modelPrefix(modelParam)))
		return
	}
	if !s.resolveConnection() {
		return
	}

	_ = s.send(map[string]any{
		"type": "connected", "provider": s.provider, "model": s.model,
		"language": s.language, "sampleRate": s.sampleRate, "encoding": s.encoding,
	})

	if err := s.dispatch(); err != nil {
		s.errAndClose(4502, "Upstream connection failed", "Upstream error: "+err.Error())
		return
	}

	s.readLoop()
	s.cleanup()
}

type urlValues map[string][]string

// get returns the first value of key, or def when absent or empty.
func (q urlValues) get(key, def string) string {
	if v, ok := q[key]; ok && len(v) > 0 && v[0] != "" {
		return v[0]
	}
	return def
}

// parseRate parses the sample rate; an unparsable or non-positive value falls
// back to the rate every backend coerces to anyway (upstream's
// `sampleRate || 16000`).
func parseRate(raw string) int {
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 16000
	}
	return n
}

// parseModel splits "provider/model" and canonicalizes the provider through
// the shared alias table. Upstream also resolves combos there; a combo or a
// bare name has no provider either, and fails with the same Invalid model
// shape.
func (s *session) parseModel(modelParam string) bool {
	prefix, model, ok := splitModel(modelParam)
	if !ok {
		s.errAndClose(4400, "Invalid model", "Invalid model specification: "+modelParam)
		return false
	}
	s.provider = providers.ResolveAlias(prefix)
	s.model = model
	return true
}

// modelPrefix recovers the spelling the client used, for the
// unsupported-provider message (upstream names the param, not the canonical).
func modelPrefix(modelParam string) string {
	prefix, _, _ := splitModel(modelParam)
	return prefix
}

func splitModel(modelParam string) (prefix, model string, ok bool) {
	parts := strings.SplitN(modelParam, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// resolveConnection picks one active connection for the provider.
//
// shortcut: no mid-session account rotation for a socket session, upgrade when
// a rotating account appears. Proxy-pool dialing exists: the pinned connection
// also resolves its outbound dialer through the chat lane's chain, and a pool
// the WebSocket cannot use kills the session before any upstream dial. The
// first active connection carrying a credential wins; without one the session
// is refused with the 4503 shape upstream uses for "no credentials".
func (s *session) resolveConnection() bool {
	conns, err := s.h.repo.GetProviderConnections(s.provider, true)
	if err != nil || len(conns) == 0 {
		s.noCredentials(err)
		return false
	}
	for _, conn := range conns {
		connData := &shared.ConnectionData{}
		if err := json.Unmarshal([]byte(conn.Data), connData, handlerutil.UpstreamBody); err != nil {
			continue
		}
		if s.h.hasCredential(connData) {
			s.connID, s.connData = conn.ID, connData
			return s.wireDialer()
		}
	}
	s.noCredentials(nil)
	return false
}

// wireDialer resolves the pinned connection's outbound-proxy dialer. A
// connection bound to a pool the WebSocket cannot use — a strict edge relay,
// a missing or inactive pool — must refuse the session here: falling back to
// a direct dial would leak the operator's egress IP, the one thing the proxy
// assignment exists to prevent.
func (s *session) wireDialer() bool {
	if s.h.dialFor == nil {
		return true
	}
	d, err := s.h.dialFor(s.connData)
	if err != nil {
		msg := fmt.Sprintf("No usable proxy for %s: %v", s.provider, err)
		// The close reason is capped by the WS spec at 123 bytes; the full
		// detail rides the error frame above, not the close.
		s.errAndClose(4503, "No usable proxy for "+s.provider, msg)
		return false
	}
	s.dial = d
	return true
}

// noCredentials sends the error frame and the 4503 close upstream uses when
// getProviderCredentials returns nothing usable. Upstream carries the
// connection's lastError when rotation produced one; the session shortcut
// always has the plain default message.
func (s *session) noCredentials(err error) {
	msg := fmt.Sprintf("No credentials configured for provider: %s", s.provider)
	if err != nil {
		msg = fmt.Sprintf("no credentials for %s: %v", s.provider, err)
	}
	s.errAndClose(4503, msg, msg)
}

// errAndClose sends one error text frame then the close frame.
func (s *session) errAndClose(code int, reason, message string) {
	_ = s.send(map[string]any{"type": "error", "error": message})
	closeWith(s.client, code, reason)
}

// send writes one gateway→client text frame. gorilla allows one concurrent
// writer, and the upstream reader and client reader both produce frames.
func (s *session) send(v any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.client.WriteJSON(v)
}

// dispatch wires up the upstream backend for the canonical provider id. The
// registry's liveCaptionConfig is declarative metadata; the branch on the id
// is what upstream executes.
func (s *session) dispatch() error {
	switch s.provider {
	case "deepgram":
		return s.setupDeepgram()
	case "elevenlabs":
		return s.setupElevenLabs()
	case "gemini":
		return s.setupGemini()
	default:
		return s.setupSelfhosted()
	}
}

// readLoop consumes the client socket until it closes or errors. Binary
// frames are raw audio; text frames are JSON control commands.
func (s *session) readLoop() {
	for {
		mt, data, err := s.client.ReadMessage()
		if err != nil {
			return
		}
		if mt == websocket.BinaryMessage {
			if f := s.forward; f != nil {
				f(data)
			}
			continue
		}
		s.command(data)
	}
}

// command handles one client text frame. Unparseable text is silently
// ignored, and unknown commands fall through — both as upstream does.
func (s *session) command(data []byte) {
	var cmd struct {
		Type   string `json:"type"`
		Format string `json:"format"`
	}
	if err := json.Unmarshal(data, &cmd, handlerutil.ClientBody); err != nil {
		return
	}
	switch cmd.Type {
	case "keepalive":
		if s.keepalive != nil {
			s.keepalive()
		}
	case "finalize":
		if s.finalize != nil {
			s.finalize()
		}
	case "get_subtitles":
		s.sendSubtitles(cmd.Format)
	}
}

// sendSubtitles re-chunks the session's committed words and answers with the
// subtitles frame.
func (s *session) sendSubtitles(format string) {
	if format == "" {
		format = "all"
	}
	chunks := s.Chunks()
	_ = s.send(map[string]any{
		"type": "subtitles", "format": format,
		"srt": ChunksToSrt(chunks), "ass": ChunksToAss(chunks), "chunks": chunks,
	})
}

// Chunks snapshots the committed words and chunks them.
func (s *session) Chunks() []Chunk {
	s.wordsMu.Lock()
	defer s.wordsMu.Unlock()
	return ChunkWords(s.words)
}

// appendWord records a committed word and emits its `word` frame. The frame
// is the Word struct itself so absent confidence stays absent (Gemini), and
// only Deepgram's `isFinal` rides along.
func (s *session) appendWord(w Word) {
	s.wordsMu.Lock()
	s.words = append(s.words, w)
	s.wordsMu.Unlock()
	_ = s.send(struct {
		Type string `json:"type"`
		Word
	}{"word", w})
}

// spawn runs fn on a tracked goroutine so cleanup can join it before the
// handler returns.
func (s *session) spawn(fn func()) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		fn()
	}()
}

// cleanup runs once when the client read loop ends: it cancels the session
// context (which unblocks every goroutine), closes the upstream, records one
// usage row, and joins the spawned goroutines — the usage row is written
// before the join so no session goroutine can outlive the caller's close.
func (s *session) cleanup() {
	s.cancel()
	s.upMu.Lock()
	up := s.up
	s.upMu.Unlock()
	if up != nil {
		_ = up.Close()
	}

	s.wordsMu.Lock()
	count := len(s.words)
	s.wordsMu.Unlock()

	connKey := ""
	if s.connData != nil {
		connKey = s.connData.APIKey
		if connKey == "" {
			connKey = s.connData.AccessToken
		}
	}
	meta := fmt.Sprintf(`{"provider":%q,"model":%q,"connectionId":%q}`, s.provider, s.model, s.connID)
	tokensJSON := fmt.Sprintf(`{"prompt_tokens":%d,"completion_tokens":0,"total_tokens":%d}`, count, count)
	if err := s.h.repo.InsertUsageHistory(s.provider, s.model, s.connID, maskKey(connKey),
		captionEndpoint, count, 0, 0, "success", count, meta, tokensJSON, s.clientK); err != nil {
		log.Error("caption", "usage insert failed", "error", err)
	}

	s.wg.Wait()
}

// hasCredential reports whether a connection carries a usable credential.
func (h *Handler) hasCredential(connData *shared.ConnectionData) bool {
	return connData.APIKey != "" || connData.AccessToken != ""
}

package realtime

import (
	"encoding/base64"
	json "encoding/json/v2"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"9router/proxy/internal/log"
	"github.com/gorilla/websocket"
)

// Gemini Live BidiGenerateContent adapter. The wire format is locked by
// upstream's tests/unit/gemini-realtime-wire.test.js:
//   - setup carries TOP-LEVEL inputAudioTranscription / outputAudioTranscription
//     (siblings of generationConfig, never nested inside it);
//   - client audio rides realtimeInput.audio{mimeType,data} — never mediaChunks;
//   - a 125 ms silence heartbeat (2048 zero bytes) feeds Gemini's server VAD,
//     which otherwise never closes a turn on speech bursts alone.
const (
	defaultBidiURL = "wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent"
	// silenceFrameBytes is 0.125 s of 16 kHz PCM16 silence.
	silenceFrameBytes = 2048
)

var silenceFrameB64 = base64.StdEncoding.EncodeToString(make([]byte, silenceFrameBytes))

// realtimeDialer is shared: one pooled dialer instead of a per-session one.
var realtimeDialer = websocket.Dialer{}

// formatGeminiModel strips provider/model prefixes and maps deprecated
// preview models onto the current live model, then re-adds models/.
func formatGeminiModel(modelName string) string {
	if modelName == "" {
		return "models/gemini-3.8-live"
	}
	clean := modelName
	clean = strings.TrimPrefix(clean, "gemini/")
	clean = strings.TrimPrefix(clean, "models/")
	switch clean {
	case "gemini-2.0-flash-exp", "gemini-2.0-flash-realtime", "gemini-2.5-flash-native-audio-preview-09-17":
		clean = "gemini-3.8-live"
	}
	return "models/" + clean
}

type geminiAdapter struct {
	c       *adapterCtx
	dialURL string
	// up is published through an atomic pointer: Connect runs on the dial
	// goroutine while the client read loop may already be sending events, and
	// the pre-connect nil must be observable without a data race.
	up        atomic.Pointer[upstreamSocket]
	setupSent atomic.Bool

	// mu guards the mutable setup state: voice/instructions are updated from
	// the client read loop while the setup frame may be sent from the
	// upstream-open moment or the client read loop itself.
	mu          sync.Mutex
	activeVoice string
	activeInstr string
	sessionID   string
	responseID  string
	itemID      string
}

func newGeminiAdapter(c *adapterCtx) (Adapter, error) {
	base := c.overrideURL()
	if base == "" {
		base = c.cfg.WsURL
	}
	if base == "" {
		base = defaultBidiURL
	}
	param := orDefaultStr(c.cfg.AuthQueryParam, "key")
	if c.cfg.AudioOutRate > 0 {
		c.setOutputRate(c.cfg.AudioOutRate)
	}
	voice := c.voice
	if voice == "" {
		voice = "Puck"
	}
	return &geminiAdapter{
		c:           c,
		dialURL:     toWSDialURL(base) + "?" + param + "=" + url.QueryEscape(c.apiKey),
		activeVoice: voice,
		activeInstr: c.instructions,
		sessionID:   newID("sess_"),
		responseID:  newID("resp_"),
		itemID:      newID("item_"),
	}, nil
}

func (g *geminiAdapter) Connect() error {
	wsConn, _, err := realtimeDialer.DialContext(g.c.sess.ctx, g.dialURL, nil) //nolint:bodyclose // gorilla: on success the handshake body IS the socket, on failure it need not be closed.
	if err != nil {
		return err
	}
	g.up.Store(&upstreamSocket{conn: wsConn})
	g.sendSetup()
	g.startSilenceHeartbeat()
	g.c.sess.wg.Add(1)
	go func() {
		defer g.c.sess.wg.Done()
		g.readUpstream(wsConn)
	}()
	return nil
}

// sendSetup sends the setup frame once. It is sent immediately when the
// upstream opens, or lazily before the first audio / on a pre-open
// session.update.
func (g *geminiAdapter) sendSetup() {
	up := g.up.Load()
	if !up.isOpen() || !g.setupSent.CompareAndSwap(false, true) {
		return
	}

	g.mu.Lock()
	voice, instr, model := g.activeVoice, g.activeInstr, g.c.model
	g.mu.Unlock()

	frame := map[string]any{
		"setup": map[string]any{
			"model": formatGeminiModel(model),
			"generationConfig": map[string]any{
				"responseModalities": []string{"AUDIO"},
				"speechConfig": map[string]any{
					"voiceConfig": map[string]any{
						"prebuiltVoiceConfig": map[string]any{"voiceName": voice},
					},
				},
			},
			"systemInstruction": map[string]any{
				"parts": []any{map[string]any{"text": instr}},
			},
			"inputAudioTranscription":  map[string]any{},
			"outputAudioTranscription": map[string]any{},
		},
	}
	if err := up.sendJSON(frame); err != nil {
		log.Warn("REALTIME_VOICE", "gemini setup send failed", "error", err)
	}
}

func (g *geminiAdapter) forwardAudio(b64 string) {
	if !g.setupSent.Load() {
		g.sendSetup()
	}
	if up := g.up.Load(); up.isOpen() {
		_ = up.sendJSON(map[string]any{
			"realtimeInput": map[string]any{
				"audio": map[string]any{"mimeType": "audio/pcm;rate=16000", "data": b64},
			},
		})
	}
}

func (g *geminiAdapter) sendClientContent(parts []any) {
	if up := g.up.Load(); up.isOpen() {
		_ = up.sendJSON(map[string]any{
			"clientContent": map[string]any{"turns": []any{map[string]any{"role": "user", "parts": parts}}, "turnComplete": true},
		})
	}
}

// startSilenceHeartbeat feeds Gemini a continuous silence track (0.125 s
// frames) from upstream open, like a live mic would. Exits with the session
// context; the isOpen guard drops late ticks between close and cancellation.
func (g *geminiAdapter) startSilenceHeartbeat() {
	g.c.sess.wg.Add(1)
	go func() {
		defer g.c.sess.wg.Done()
		ticker := time.NewTicker(125 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-g.c.sess.ctx.Done():
				return
			case <-ticker.C:
				if up := g.up.Load(); g.setupSent.Load() && up.isOpen() {
					_ = up.sendJSON(map[string]any{
						"realtimeInput": map[string]any{
							"audio": map[string]any{"mimeType": "audio/pcm;rate=16000", "data": silenceFrameB64},
						},
					})
				}
			}
		}
	}()
}

func (g *geminiAdapter) readUpstream(wsConn *websocket.Conn) {
	for {
		_, data, err := wsConn.ReadMessage()
		if err != nil {
			// Any read termination is an upstream close from the session's
			// point of view: close the client with 1000 "Upstream closed"
			// when it is still open (teardown already ran otherwise).
			if g.c.isClientOpen() {
				g.c.closeClient(1000, "Upstream closed")
			}
			return
		}
		var msg map[string]any
		if err := json.Unmarshal(data, &msg); err != nil {
			log.Warn("REALTIME_VOICE", "dropping unparseable gemini frame")
			continue
		}
		g.handleUpstreamFrame(msg)
	}
}

func (g *geminiAdapter) handleUpstreamFrame(msg map[string]any) {
	if msgErr, ok := msg["error"].(map[string]any); ok {
		message, _ := msgErr["message"].(string)
		if message == "" {
			message = "Gemini Live error"
		}
		sendClientError(g.c, message)
		return
	}
	if _, ok := msg["setupComplete"]; ok {
		g.mu.Lock()
		voice, model := g.activeVoice, g.c.model
		g.mu.Unlock()
		sendSessionCreated(g.c, g.sessionID, model, voice)
		sendSessionUpdated(g.c, g.sessionID, model, voice)
		return
	}
	if sc, ok := msg["serverContent"].(map[string]any); ok {
		g.handleServerContent(sc)
	}
}

func (g *geminiAdapter) handleServerContent(sc map[string]any) {
	if _, interrupted := sc["interrupted"]; interrupted {
		g.mu.Lock()
		responseID, itemID := g.responseID, g.itemID
		g.mu.Unlock()
		sendSpeechStarted(g.c, itemID, g.c.millisSinceStart())
		sendResponseCancelled(g.c, responseID)
		g.rotateIDs()
		return
	}
	if ot, ok := sc["outputTranscription"].(map[string]any); ok {
		if text, _ := ot["text"].(string); text != "" {
			g.mu.Lock()
			responseID, itemID := g.responseID, g.itemID
			g.mu.Unlock()
			sendTranscriptDelta(g.c, responseID, itemID, text)
		}
	}
	if mt, ok := sc["modelTurn"].(map[string]any); ok {
		g.handleModelTurn(mt)
	}
	if _, complete := sc["turnComplete"]; complete {
		g.mu.Lock()
		responseID, itemID := g.responseID, g.itemID
		g.mu.Unlock()
		sendTurnComplete(g.c, responseID, itemID)
		g.rotateIDs()
	}
}

func (g *geminiAdapter) handleModelTurn(mt map[string]any) {
	parts, _ := mt["parts"].([]any)
	for _, rawPart := range parts {
		part, ok := rawPart.(map[string]any)
		if !ok {
			continue
		}
		if inline, ok := part["inlineData"].(map[string]any); ok {
			if b64, _ := inline["data"].(string); b64 != "" {
				g.c.countOutput(decodedAudioLen(b64))
				g.mu.Lock()
				responseID, itemID := g.responseID, g.itemID
				g.mu.Unlock()
				sendAudioDelta(g.c, responseID, itemID, b64)
			}
		}
		if text, _ := part["text"].(string); text != "" {
			g.mu.Lock()
			responseID, itemID := g.responseID, g.itemID
			g.mu.Unlock()
			sendTranscriptDelta(g.c, responseID, itemID, text)
		}
	}
}

func (g *geminiAdapter) rotateIDs() {
	g.mu.Lock()
	g.responseID, g.itemID = newID("resp_"), newID("item_")
	g.mu.Unlock()
}

func (g *geminiAdapter) HandleClientEvent(event map[string]any) {
	switch eventType(event) {
	case "session.update":
		g.handleSessionUpdate(event)
	case "input_audio_buffer.append":
		if audio, _ := event["audio"].(string); audio != "" {
			g.forwardAudio(audio)
		}
	case "input_audio_buffer.commit", "response.create":
		g.sendClientContent([]any{map[string]any{"text": ""}})
	case "conversation.item.create":
		g.handleItemCreate(event)
	}
}

func (g *geminiAdapter) handleSessionUpdate(event map[string]any) {
	session, _ := event["session"].(map[string]any)
	g.mu.Lock()
	if voice, _ := session["voice"].(string); voice != "" {
		g.activeVoice = voice
	}
	if instr, _ := session["instructions"].(string); instr != "" {
		g.activeInstr = instr
	}
	voice := g.activeVoice
	g.mu.Unlock()

	if !g.setupSent.Load() {
		g.sendSetup()
		return
	}
	sendSessionUpdated(g.c, g.sessionID, g.c.model, voice)
}

func (g *geminiAdapter) handleItemCreate(event map[string]any) {
	item, _ := event["item"].(map[string]any)
	content, _ := item["content"].([]any)
	parts := make([]any, 0, len(content))
	for _, raw := range content {
		c, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := c["type"].(string); t == "input_text" {
			if text, _ := c["text"].(string); text != "" {
				parts = append(parts, map[string]any{"text": text})
			}
		}
	}
	if len(parts) > 0 {
		g.sendClientContent(parts)
	}
}

func (g *geminiAdapter) HandleClientAudio(data []byte) {
	g.forwardAudio(base64.StdEncoding.EncodeToString(data))
}

func (g *geminiAdapter) Close() {
	g.up.Load().close(1000, "Client closed")
}

func eventType(event map[string]any) string {
	t, _ := event["type"].(string)
	return t
}

// decodedAudioLen returns the exact number of bytes a standard-alphabet base64
// audio chunk decodes to, without allocating on the streaming path.
func decodedAudioLen(b64 string) int {
	n := len(b64)
	pad := 0
	switch {
	case strings.HasSuffix(b64, "=="):
		pad = 2
	case strings.HasSuffix(b64, "="):
		pad = 1
	}
	return n/4*3 - pad
}

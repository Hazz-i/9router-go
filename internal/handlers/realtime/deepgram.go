package realtime

import (
	"encoding/base64"
	json "encoding/json/v2"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"9router/proxy/internal/log"
	"github.com/gorilla/websocket"
)

// Deepgram Voice Agent adapter (wss://agent.deepgram.com/v1/agent/converse):
// the first upstream frame MUST be Settings, client audio goes up as raw
// binary PCM frames, and audio comes back as binary (the only adapter here
// with binary upstream), mapped to response.audio.delta.
const defaultDeepgramWsURL = "wss://agent.deepgram.com/v1/agent/converse"

type deepgramAdapter struct {
	c           *adapterCtx
	up          *upstreamSocket
	ready       atomic.Bool
	keepAliveMs int

	listenFrag map[string]any
	thinkFrag  map[string]any // the override/default `provider` fragment; prompt+endpoint are merged at Settings time
	speakFrag  map[string]any
	greeting   string

	// instructionsMu guards the prompt the Settings frame picks up: it is
	// written from the client read loop and read at Settings-build time.
	instructionsMu sync.Mutex
	instructions   string
	sessionID      string
	// responseID/itemID are only touched by the upstream read pump (mapped
	// events and rotation), so they need no extra lock.
	responseID string
	itemID     string
}

func newDeepgramAdapter(c *adapterCtx) (Adapter, error) {
	// ?listen=/?think=/?speak= resolution runs HERE, before any upstream dial:
	// an unknown choice must fail as realtime_config_error + close 4400 with
	// zero sockets opened (Lane A's Resolve reproduces upstream's resolver).
	listenFrag, err := resolveChoice(c.cfg.Listen, "listen", c.query.Get("listen"))
	if err != nil {
		return nil, err
	}
	thinkFrag, err := resolveChoice(c.cfg.Think, "think", c.query.Get("think"))
	if err != nil {
		return nil, err
	}
	speakFrag, err := resolveChoice(c.cfg.Speak, "speak", c.query.Get("speak"))
	if err != nil {
		return nil, err
	}

	keepAliveMs := c.cfg.KeepAliveMs
	if keepAliveMs <= 0 {
		keepAliveMs = 5000
	}
	return &deepgramAdapter{
		c:            c,
		listenFrag:   listenFrag,
		thinkFrag:    thinkFrag,
		speakFrag:    speakFrag,
		greeting:     c.query.Get("greeting"),
		keepAliveMs:  keepAliveMs,
		instructions: c.instructions,
		sessionID:    newID("sess_"),
		responseID:   newID("resp_"),
		itemID:       newID("item_"),
	}, nil
}

// resolveChoice delegates ?kind=<type>/<id> to Lane A's RealtimeChoiceSet
// (upstream's resolveOverride); a nil set or empty param means no override.
func resolveChoice(set interface {
	Resolve(kind, raw string) (map[string]any, error)
}, kind, raw string) (map[string]any, error) {
	if set == nil || raw == "" {
		return nil, nil
	}
	frag, err := set.Resolve(kind, raw)
	if err != nil {
		return nil, err
	}
	return frag, nil
}

// buildSettings builds the first upstream frame. Optional override data
// (providers groups, customVoice) never reaches upstream — only the resolved
// fragments, prompt, optional endpoint hop and greeting do.
//
// The think endpoint hop rides ONLY when providerSpecificData carries both
// thinkApiKey and thinkUrl: Deepgram runs its built-in think providers on its
// own credentials, so no think credentials are required to connect.
func (d *deepgramAdapter) buildSettings() map[string]any {
	psd := map[string]any{}
	if d.c.connData != nil && d.c.connData.ProviderSpecificData != nil {
		psd = d.c.connData.ProviderSpecificData
	}
	thinkURL, _ := psd["thinkUrl"].(string)
	thinkKey, _ := psd["thinkApiKey"].(string)

	d.instructionsMu.Lock()
	instructions := d.instructions
	d.instructionsMu.Unlock()

	listen := d.listenFrag
	if listen == nil {
		listen = map[string]any{"provider": d.c.cfg.Listen.Provider}
	}
	think := map[string]any{}
	for k, v := range d.thinkFrag {
		think[k] = v
	}
	if think["provider"] == nil {
		think["provider"] = d.c.cfg.Think.Provider
	}
	think["prompt"] = instructions
	if thinkURL != "" && thinkKey != "" {
		think["endpoint"] = map[string]any{
			"url":     thinkURL,
			"headers": map[string]any{"Authorization": fmt.Sprintf("Bearer %s", thinkKey)},
		}
	}
	speak := d.speakFrag
	if speak == nil {
		speak = map[string]any{"provider": d.c.cfg.Speak.Provider}
	}

	agent := map[string]any{"listen": listen, "think": think, "speak": speak}
	if d.greeting != "" {
		agent["greeting"] = d.greeting
	}
	return map[string]any{
		"type": "Settings",
		"audio": map[string]any{
			"input": map[string]any{
				"encoding":    "linear16",
				"sample_rate": orDefault(d.c.cfg.AudioInRate, 16000),
			},
			"output": map[string]any{
				"encoding":    "linear16",
				"sample_rate": orDefault(d.c.cfg.AudioOutRate, 24000),
				"container":   "none",
			},
		},
		"agent": agent,
	}
}

func orDefault(v, def int) int {
	return orDefaultInt(v, def)
}

func (d *deepgramAdapter) dialURL() string {
	if u := d.c.overrideURL(); u != "" {
		return toWSDialURL(u)
	}
	if u := d.c.cfg.WsURL; u != "" {
		return toWSDialURL(u)
	}
	return toWSDialURL(defaultDeepgramWsURL)
}

func (d *deepgramAdapter) Connect() error {
	header := http.Header{}
	header.Set(orDefaultStr(d.c.cfg.AuthHeader, "Authorization"),
		orDefaultStr(d.c.cfg.AuthScheme, "Token")+" "+d.c.apiKey)
	wsConn, _, err := realtimeDialer.DialContext(d.c.sess.ctx, d.dialURL(), header) //nolint:bodyclose // gorilla: on success the handshake body IS the socket, on failure it need not be closed.
	if err != nil {
		return err
	}
	d.up = &upstreamSocket{conn: wsConn}
	// Settings is the FIRST upstream frame — it must not race the client audio.
	if err := d.up.sendJSON(d.buildSettings()); err != nil {
		return err
	}
	d.startKeepAlive()
	d.c.sess.wg.Add(1)
	go func() {
		defer d.c.sess.wg.Done()
		d.readUpstream(wsConn)
	}()
	return nil
}

// startKeepAlive sends {type:"KeepAlive"} every keepAliveMs while the upstream
// is open. Exits with the session context.
func (d *deepgramAdapter) startKeepAlive() {
	d.c.sess.wg.Add(1)
	go func() {
		defer d.c.sess.wg.Done()
		ticker := time.NewTicker(time.Duration(d.keepAliveMs) * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-d.c.sess.ctx.Done():
				return
			case <-ticker.C:
				if d.up.isOpen() {
					_ = d.up.sendJSON(map[string]any{"type": "KeepAlive"})
				}
			}
		}
	}()
}

func (d *deepgramAdapter) readUpstream(wsConn *websocket.Conn) {
	for {
		msgType, data, err := wsConn.ReadMessage()
		if err != nil {
			// Upstream gone: close the client with 1000 "Upstream closed"
			// when it is still open (after teardown this is a no-op).
			if d.c.isClientOpen() {
				d.c.closeClient(1000, "Upstream closed")
			}
			return
		}
		if msgType == websocket.BinaryMessage {
			// Raw linear16 PCM — base64 it into response.audio.delta.
			d.c.countOutput(len(data))
			sendAudioDelta(d.c, d.responseID, d.itemID, base64.StdEncoding.EncodeToString(data))
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal(data, &msg); err != nil {
			log.Warn("REALTIME_VOICE", "dropping unparseable deepgram frame")
			continue
		}
		d.handleUpstreamFrame(msg)
	}
}

func (d *deepgramAdapter) handleUpstreamFrame(msg map[string]any) {
	switch eventType(msg) {
	case "Welcome":
		// Nothing client-facing.
	case "SettingsApplied":
		// Only the first one opens the session; no voice field — Deepgram
		// carries no default voice.
		if d.ready.CompareAndSwap(false, true) {
			sendSessionCreated(d.c, d.sessionID, d.c.model, "")
			sendSessionUpdated(d.c, d.sessionID, d.c.model, "")
		}
	case "ConversationText":
		if role, _ := msg["role"].(string); role == "assistant" {
			if content, _ := msg["content"].(string); content != "" {
				sendTranscriptDelta(d.c, d.responseID, d.itemID, content)
			}
		}
	case "UserStartedSpeaking":
		sendSpeechStarted(d.c, d.itemID, -1)
		sendResponseCancelled(d.c, d.responseID)
		d.rotateIDs()
	case "AgentThinking", "AgentStartedSpeaking":
		// No client protocol event; the audio deltas carry the output.
	case "AgentAudioDone":
		sendTurnComplete(d.c, d.responseID, d.itemID)
		d.rotateIDs()
	case "Error":
		description, _ := msg["description"].(string)
		if description == "" {
			description = "Deepgram agent error"
		}
		if v := msg["code"]; v != nil {
			// Upstream templates the code verbatim (numbers included).
			description = fmt.Sprint(v) + ": " + description
		}
		sendClientError(d.c, description)
	}
}

func (d *deepgramAdapter) rotateIDs() {
	d.responseID, d.itemID = newID("resp_"), newID("item_")
}

func (d *deepgramAdapter) HandleClientEvent(event map[string]any) {
	switch eventType(event) {
	case "session.update":
		session, _ := event["session"].(map[string]any)
		// Applied to the next Settings prompt when it has not been sent yet;
		// afterwards it is only echoed, never re-sent upstream.
		if instr, _ := session["instructions"].(string); instr != "" {
			d.instructionsMu.Lock()
			d.instructions = instr
			d.instructionsMu.Unlock()
		}
		if d.ready.Load() {
			sendSessionUpdated(d.c, d.sessionID, d.c.model, "")
		}
	case "input_audio_buffer.append":
		if audio, _ := event["audio"].(string); audio != "" && d.up.isOpen() {
			if b, err := base64.StdEncoding.DecodeString(audio); err == nil {
				_ = d.up.sendBinary(b)
			}
		}
	}
}

func (d *deepgramAdapter) HandleClientAudio(data []byte) {
	if d.up.isOpen() {
		_ = d.up.sendBinary(data)
	}
}

func (d *deepgramAdapter) Close() {
	d.up.close(1000, "Client closed")
}

// ElevenLabs Scribe backend: base64-wrapped JSON audio chunks over
// wss://api.elevenlabs.io/v1/speech-to-text/realtime.
package caption

import (
	"encoding/base64"
	json "encoding/json/v2"
	"errors"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"9router/proxy/internal/handlerutil"
	"9router/proxy/internal/log"
)

// setupElevenLabs dials the Scribe realtime endpoint. Upstream hardcodes the
// realtime model id: whatever model the query carried, the wire always asks
// for scribe_v2_realtime.
func (s *session) setupElevenLabs() error {
	key := s.connData.APIKey
	if key == "" {
		key = s.connData.AccessToken
	}
	if key == "" {
		return errors.New("Missing ElevenLabs API key") //nolint:staticcheck // this text is the frame payload a client receives; it is ported verbatim from upstream.
	}

	sampleRate := s.sampleRate
	if sampleRate == 0 {
		sampleRate = 16000
	}
	params := url.Values{
		"model_id":           {"scribe_v2_realtime"},
		"audio_format":       {"pcm_" + strconv.Itoa(sampleRate)},
		"include_timestamps": {"true"},
	}
	// `auto` lets Scribe detect the language, so no language_code is sent.
	if s.language != "" && s.language != "auto" {
		params.Set("language_code", s.language)
	}

	header := http.Header{"xi-api-key": {key}}
	s.dialUpstream("ElevenLabs Scribe", s.h.elevenlabsURL+"?"+params.Encode(), header)
	if !s.upstreamOpen() {
		return nil
	}

	s.forward = func(data []byte) {
		s.sendUpstream(map[string]any{
			"message_type":  "input_audio_chunk",
			"audio_base_64": base64.StdEncoding.EncodeToString(data),
		})
	}
	s.finalize = func() {
		s.sendUpstream(map[string]any{
			"message_type":  "input_audio_chunk",
			"audio_base_64": "",
			"commit":        true,
		})
	}

	s.spawn(s.pumpElevenLabs)
	return nil
}

// pumpElevenLabs reads Scribe frames until the socket dies or the session
// ends. `ready` only fires once the upstream acknowledges the session.
func (s *session) pumpElevenLabs() {
	up := s.currentUpstream()
	if up == nil {
		return
	}
	defer s.markUpstreamClosed()

	currentStart := 0.0
	for {
		_, data, err := up.ReadMessage()
		if err != nil {
			if s.ctx.Err() == nil {
				s.announceUpstreamClosed(1006, "")
			}
			return
		}
		currentStart = s.elevenLabsMessage(data, currentStart)
	}
}

// elevenLabsMessage translates one Scribe frame; currentStart carries the
// synthesized monotonic clock for transcripts without timestamps.
func (s *session) elevenLabsMessage(data []byte, currentStart float64) float64 {
	var msg elMessage
	if err := json.Unmarshal(data, &msg, handlerutil.UpstreamBody); err != nil {
		log.Error("caption", "ElevenLabs Scribe parse error", "error", err)
		return currentStart
	}

	switch msg.MessageType {
	case "session_started":
		_ = s.send(map[string]any{"type": "ready", "upstream": "elevenlabs-scribe"})
	case "partial_transcript":
		if text := strings.TrimSpace(msg.Text); text != "" {
			_ = s.send(map[string]any{"type": "partial", "text": text, "isFinal": false})
		}
	case "committed_transcript_with_timestamps":
		currentStart = s.commitTimestamped(msg, currentStart)
	case "committed_transcript":
		currentStart = s.commitPlain(msg, currentStart)
	}
	return currentStart
}

// commitTimestamped ports the committed_transcript_with_timestamps branch:
// word-typed entries with real timings when present, confidence from the
// logprob clamped to 1, and one chunk spanning the committed text.
func (s *session) commitTimestamped(msg elMessage, currentStart float64) float64 {
	var words []elWord
	for _, w := range msg.Words {
		if w.Type == "word" && strings.TrimSpace(w.Text) != "" {
			words = append(words, w)
		}
	}

	for _, w := range words {
		start := orFloat(w.Start, currentStart)
		end := orFloat(w.End, currentStart+0.3)
		confidence := 0.95
		if w.LogProb != nil {
			confidence = math.Min(1, math.Exp(*w.LogProb))
		}
		wordObj := Word{Word: strings.TrimSpace(w.Text), Start: start, End: end, Confidence: confidence, IsFinal: true}
		currentStart = math.Max(currentStart, end)
		s.appendWord(wordObj)
	}

	if text := strings.TrimSpace(msg.Text); text != "" {
		start := currentStart
		end := currentStart
		if len(words) > 0 {
			start = orFloat(words[0].Start, currentStart)
			end = orFloat(words[len(words)-1].End, currentStart)
		}
		_ = s.send(map[string]any{"type": "chunk", "chunk": Chunk{Text: text, Start: start, End: end}})
	}
	return currentStart
}

// commitPlain ports the committed_transcript branch: no upstream timestamps,
// so each word gets 0.3 s and the chunk at least 0.4 s, advancing a
// synthesized monotonic clock.
func (s *session) commitPlain(msg elMessage, currentStart float64) float64 {
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return currentStart
	}
	tokens := strings.Fields(text)
	duration := math.Max(0.4, float64(len(tokens))*0.3)
	chunk := Chunk{Text: text, Start: currentStart, End: currentStart + duration}

	for _, token := range tokens {
		s.appendWord(Word{
			Word: token, Start: currentStart, End: currentStart + 0.3,
			Confidence: 0.95, IsFinal: true,
		})
	}
	currentStart += duration
	_ = s.send(map[string]any{"type": "chunk", "chunk": chunk})
	return currentStart
}

// orFloat returns def when the optional timestamp is absent.
func orFloat(v *float64, def float64) float64 {
	if v == nil {
		return def
	}
	return *v
}

// elWord is one committed word entry of a Scribe frame.
type elWord struct {
	Type    string   `json:"type"`
	Text    string   `json:"text"`
	Start   *float64 `json:"start"`
	End     *float64 `json:"end"`
	LogProb *float64 `json:"logprob"`
}

// elMessage is the subset of a Scribe realtime frame this session uses.
type elMessage struct {
	MessageType string   `json:"message_type"`
	Text        string   `json:"text"`
	SessionID   string   `json:"session_id"`
	Words       []elWord `json:"words"`
}

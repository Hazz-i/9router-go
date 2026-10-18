// Gemini Live backend: base64 realtimeInput frames over the BidiGenerateContent
// socket, with a gateway-sent setup frame on open.
package caption

import (
	"encoding/base64"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"9router/proxy/internal/handlerutil"
	"9router/proxy/internal/log"
)

// geminiLiveDefaultModel is the model upstream hardcodes the setup frame to.
const geminiLiveDefaultModel = "gemini-3.8-live"

// geminiLiveModel resolves the requested model: legacy live ids remap onto the
// default, exactly as upstream does.
func geminiLiveModel(model string) string {
	switch model {
	case "", "gemini-2.0-flash-exp", "gemini-2.0-flash-realtime",
		"gemini-2.5-flash-native-audio-preview-09-17":
		return geminiLiveDefaultModel
	}
	return model
}

// setupGemini dials the BidiGenerateContent socket and sends the
// transcription setup frame on open. No client frame is required first.
func (s *session) setupGemini() error {
	key := s.connData.APIKey
	if key == "" {
		key = s.connData.AccessToken
	}
	if key == "" {
		return errors.New("Missing Gemini API key") //nolint:staticcheck // this text is the frame payload a client receives; it is ported verbatim from upstream.
	}

	wsURL := s.h.geminiURL + "?key=" + url.QueryEscape(key)
	s.dialUpstream("Gemini", wsURL, nil)
	if !s.upstreamOpen() {
		return nil
	}

	s.sendUpstream(map[string]any{"setup": geminiSetup(geminiLiveModel(s.model), s.language)})
	_ = s.send(map[string]any{"type": "ready", "upstream": "gemini-live"})

	s.forward = func(data []byte) {
		// The wire is 16 kHz PCM regardless of the negotiated sample rate.
		s.sendUpstream(map[string]any{
			"realtimeInput": map[string]any{
				"audio": map[string]any{
					"mimeType": "audio/pcm;rate=16000",
					"data":     base64.StdEncoding.EncodeToString(data),
				},
			},
		})
	}

	go s.pumpGemini()
	return nil
}

// geminiSetup builds the transcription setup frame: AUDIO response
// modality, the Puck voice, a verbatim-transcription instruction, and both
// transcription taps.
func geminiSetup(model, language string) map[string]any {
	instruction := "Transcribe the spoken audio verbatim."
	if language != "" {
		instruction = fmt.Sprintf("Transcribe the spoken audio verbatim in language %s.", language)
	}
	return map[string]any{
		"model": "models/" + model,
		"generationConfig": map[string]any{
			"responseModalities": []string{"AUDIO"},
			"speechConfig": map[string]any{
				"voiceConfig": map[string]any{
					"prebuiltVoiceConfig": map[string]any{"voiceName": "Puck"},
				},
			},
		},
		"systemInstruction":        map[string]any{"parts": []map[string]string{{"text": instruction}}},
		"inputAudioTranscription":  map[string]any{},
		"outputAudioTranscription": map[string]any{},
	}
}

// pumpGemini reads Gemini frames until the socket dies or the session ends.
// Transcription deltas are appended with a flat 0.4 s synthesized clock.
func (s *session) pumpGemini() {
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
		currentStart = s.geminiMessage(data, currentStart)
	}
}

// geminiMessage translates one Gemini Live frame: in-frame errors become
// error frames (no close), transcription deltas become committed words.
func (s *session) geminiMessage(data []byte, currentStart float64) float64 {
	var frame geminiFrame
	if err := json.Unmarshal(data, &frame, handlerutil.UpstreamBody); err != nil {
		log.Error("caption", "Gemini message parse error", "error", err)
		return currentStart
	}

	if frame.Error != nil {
		message := "Gemini Live error"
		if frame.Error.Message != "" {
			message = frame.Error.Message
		}
		_ = s.send(map[string]any{"type": "error", "error": message})
		return currentStart
	}

	if frame.ServerContent == nil {
		return currentStart
	}
	sc := frame.ServerContent
	var delta string
	if sc.OutputTranscription != nil {
		delta = sc.OutputTranscription.Text
	}
	if delta == "" && sc.InputTranscription != nil {
		delta = sc.InputTranscription.Text
	}
	text := strings.TrimSpace(delta)
	if text == "" {
		return currentStart
	}

	s.appendWord(Word{Word: text, Start: currentStart, End: currentStart + 0.4, IsFinal: true})
	return currentStart + 0.4
}

// geminiFrame is the subset of a Gemini Live frame this session uses.
type geminiFrame struct {
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
	ServerContent *struct {
		InputTranscription *struct {
			Text string `json:"text"`
		} `json:"inputTranscription"`
		OutputTranscription *struct {
			Text string `json:"text"`
		} `json:"outputTranscription"`
	} `json:"serverContent"`
}

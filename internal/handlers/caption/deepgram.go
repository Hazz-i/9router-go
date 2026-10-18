// Deepgram backend: raw binary passthrough over wss://api.deepgram.com/v1/listen.
package caption

import (
	json "encoding/json/v2"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"9router/proxy/internal/handlerutil"
	"9router/proxy/internal/log"

	"github.com/gorilla/websocket"
)

// setupDeepgram dials the Deepgram realtime listen endpoint and forwards
// client audio verbatim.
func (s *session) setupDeepgram() error {
	key := s.connData.APIKey
	if key == "" {
		key = s.connData.AccessToken
	}
	if key == "" {
		return errors.New("Missing Deepgram API key") //nolint:staticcheck // this text is the frame payload a client receives; it is ported verbatim from upstream.
	}

	params := url.Values{
		"model":           {orStr(s.model, "nova-3")},
		"language":        {orStr(s.language, "id")},
		"smart_format":    {"true"},
		"interim_results": {"true"},
		"punctuate":       {"true"},
		"endpointing":     {"300"},
	}
	// Only the explicitly negotiated PCM encodings set encoding/sample_rate;
	// anything else lets Deepgram auto-detect the container (e.g. webm_opus).
	if s.encoding == "linear16" || s.encoding == "pcm" {
		params.Set("encoding", "linear16")
		sampleRate := s.sampleRate
		if sampleRate == 0 {
			sampleRate = 16000
		}
		params.Set("sample_rate", strconv.Itoa(sampleRate))
	}

	header := http.Header{"Authorization": {"Token " + key}}
	s.dialUpstream("Deepgram", s.h.deepgramURL+"?"+params.Encode(), header)
	if !s.upstreamOpen() {
		return nil // dial failed: error + upstream_closed already sent
	}

	_ = s.send(map[string]any{"type": "ready", "upstream": "deepgram"})
	s.keepalive = func() { s.sendUpstream(map[string]any{"type": "KeepAlive"}) }
	s.finalize = func() { s.sendUpstream(map[string]any{"type": "Finalize"}) }
	s.forward = func(data []byte) { s.forwardBinary(websocket.BinaryMessage, data) }

	s.spawn(s.pumpDeepgram)
	return nil
}

// pumpDeepgram reads Deepgram Results frames and translates them into
// partial/word/chunk frames until the socket dies or the session ends.
func (s *session) pumpDeepgram() {
	up := s.currentUpstream()
	if up == nil {
		return
	}
	defer s.markUpstreamClosed()
	for {
		_, data, err := up.ReadMessage()
		if err != nil {
			if s.ctx.Err() == nil {
				s.announceUpstreamClosed(1006, "")
			}
			return
		}
		s.deepgramMessage(data)
	}
}

// deepgramMessage translates one Results frame. Interims ride `partial` and
// are never appended as committed words; finals are deduped against the last
// stored word so a client Finalize (which makes Deepgram re-emit the segment
// it already committed) cannot print the transcript twice.
func (s *session) deepgramMessage(data []byte) {
	var msg deepgramResults
	if err := json.Unmarshal(data, &msg, handlerutil.UpstreamBody); err != nil {
		log.Error("caption", "Deepgram message parse error", "error", err)
		return
	}
	if msg.Type != "Results" || len(msg.Channel.Alternatives) == 0 {
		return
	}
	alt := msg.Channel.Alternatives[0]
	if !msg.IsFinal {
		if strings.TrimSpace(alt.Transcript) != "" {
			_ = s.send(map[string]any{"type": "partial", "text": alt.Transcript})
		}
		return
	}

	for _, w := range alt.Words {
		if !s.acceptDeepgramWord(w) {
			continue
		}
		word := w.PunctuatedWord
		if word == "" {
			word = w.Word
		}
		s.appendWord(Word{
			Word: word, Start: w.Start, End: w.End,
			Confidence: w.Confidence, IsFinal: true,
		})
	}

	if strings.TrimSpace(alt.Transcript) != "" {
		// Upstream aggregates only the last 20 words per final.
		last := s.lastWords(20)
		chunks := ChunkWords(last)
		if len(chunks) > 0 {
			_ = s.send(map[string]any{"type": "chunk", "chunk": chunks[len(chunks)-1]})
		}
	}
}

// acceptDeepgramWord reports whether the word is new: anything at or before
// the last stored word's end is a re-report of committed material.
func (s *session) acceptDeepgramWord(w deepgramWord) bool {
	s.wordsMu.Lock()
	defer s.wordsMu.Unlock()
	lastEnd := -1.0
	if len(s.words) > 0 {
		lastEnd = s.words[len(s.words)-1].End
	}
	return w.Start > lastEnd
}

// lastWords snapshots up to n most recent committed words.
func (s *session) lastWords(n int) []Word {
	s.wordsMu.Lock()
	defer s.wordsMu.Unlock()
	if len(s.words) > n {
		return s.words[len(s.words)-n:]
	}
	return s.words
}

// deepgramResults is the subset of a Deepgram Results frame this session uses.
type deepgramResults struct {
	Type    string `json:"type"`
	IsFinal bool   `json:"is_final"`
	Channel struct {
		Alternatives []struct {
			Transcript string         `json:"transcript"`
			Words      []deepgramWord `json:"words"`
		} `json:"alternatives"`
	} `json:"channel"`
}

type deepgramWord struct {
	Word           string  `json:"word"`
	PunctuatedWord string  `json:"punctuated_word"`
	Start          float64 `json:"start"`
	End            float64 `json:"end"`
	Confidence     float64 `json:"confidence"`
}

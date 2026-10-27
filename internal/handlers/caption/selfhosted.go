// Selfhosted backend: the catch-all. Audio accumulates in a bounded window
// and is flushed as an OpenAI-compatible multipart transcription POST. This
// is the one place a caption session buffers audio; the window is capped at
// the flush threshold.
package caption

import (
	"bytes"
	json "encoding/json/v2"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"

	"9router/proxy/internal/handlerutil"
	"9router/proxy/internal/log"
)

// selfhostedFlushBytes is upstream's ~2 s of 16 kHz 16-bit mono.
const selfhostedFlushBytes = 32000 * 2

// setupSelfhosted sends `ready` immediately (there is no upstream socket) and
// buffers client audio until a flush.
func (s *session) setupSelfhosted() error {
	_ = s.send(map[string]any{"type": "ready", "upstream": "selfhosted-stt"})

	var buf bytes.Buffer
	s.forward = func(data []byte) {
		buf.Write(data)
		if buf.Len() >= selfhostedFlushBytes {
			s.flushSelfhosted(&buf)
		}
	}
	s.finalize = func() { s.flushSelfhosted(&buf) }
	return nil
}

// flushSelfhosted POSTs the buffered PCM as an audio/wav part (upstream wraps
// the raw bytes in a Blob named audio.wav with no RIFF header) and emits one
// committed word per verbose_json word. A failed build or POST is logged and
// the buffer dropped, as upstream does.
func (s *session) flushSelfhosted(buf *bytes.Buffer) {
	if buf.Len() == 0 {
		return
	}
	payload := make([]byte, buf.Len())
	copy(payload, buf.Bytes())
	buf.Reset()

	body := &bytes.Buffer{}
	form := multipart.NewWriter(body)
	file, err := form.CreateFormFile("file", "audio.wav")
	if err != nil {
		log.Error("caption", "self-hosted STT flush error", "error", err)
		return
	}
	if _, err := file.Write(payload); err != nil {
		log.Error("caption", "self-hosted STT flush error", "error", err)
		return
	}
	_ = form.WriteField("model", orStr(s.model, "whisper-1"))
	_ = form.WriteField("response_format", "verbose_json")
	if s.language != "" {
		_ = form.WriteField("language", s.language)
	}
	if err := form.Close(); err != nil {
		log.Error("caption", "self-hosted STT flush error", "error", err)
		return
	}
	s.postSelfhosted(body.Bytes(), form.FormDataContentType())
}

// postSelfhosted performs the transcription POST and translates verbose_json
// words into committed word frames.
func (s *session) postSelfhosted(body []byte, contentType string) {
	req, err := http.NewRequestWithContext(s.ctx, http.MethodPost, s.selfhostedBaseURL(), bytes.NewReader(body))
	if err != nil {
		log.Error("caption", "self-hosted STT flush error", "error", err)
		return
	}
	req.Header.Set("Content-Type", contentType)
	if key := s.connData.APIKey; key != "" && key != "none" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	client, clientErr := s.postClient()
	if clientErr != nil {
		log.Error("caption", "self-hosted STT flush error", "error", clientErr)
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		if s.ctx.Err() == nil {
			log.Error("caption", "self-hosted STT flush error", "error", err)
		}
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		log.Error("caption", "self-hosted STT flush error", "status", strconv.Itoa(resp.StatusCode))
		return
	}

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		log.Error("caption", "self-hosted STT flush error", "error", readErr)
		return
	}
	var result selfhostedResult
	if err := json.Unmarshal(raw, &result, handlerutil.UpstreamBody); err != nil {
		log.Error("caption", "self-hosted STT flush error", "error", err)
		return
	}
	for _, w := range result.Words {
		s.appendWord(Word{Word: w.Word, Start: w.Start, End: w.End, IsFinal: true})
	}
}

// postClient resolves the connection's proxy-aware HTTP client once per
// session. This is the one caption backend that sends HTTP rather than a
// WebSocket, and a connection bound to a proxy must leave through it: posting
// from the gateway's own address while the dashboard shows the connection as
// proxied is exactly the leak the assignment prevents. An unresolvable pool is
// therefore an error, never a fallback to the plain client.
//
// An edge-relay pool is refused too: a relay forwards by rewriting the request
// onto x-relay-target / x-relay-path, which only the chat lane implements, so
// this lane would otherwise dial the real base URL.
func (s *session) postClient() (*http.Client, error) {
	if s.post != nil || s.postErr != nil {
		return s.post, s.postErr
	}
	if s.h.clientFor == nil || s.connData == nil {
		return s.h.http, nil
	}
	if id := s.connData.ProxyPoolID; id != "" {
		if pool, err := s.h.repo.GetProxyPool(id); err == nil && pool != nil && pool.IsEdgeRelay() {
			s.postErr = fmt.Errorf("proxy pool %q is an edge relay and cannot carry the self-hosted transcription POST", id)
			return nil, s.postErr
		}
	}
	client, err := s.h.clientFor(s.connData)
	if err != nil {
		s.postErr = err
		return nil, err
	}
	s.post = client
	return s.post, nil
}

// selfhostedBaseURL prefers the connection's own baseUrl over the default.
func (s *session) selfhostedBaseURL() string {
	if s.connData != nil && strings.TrimSpace(s.connData.BaseURL) != "" {
		return s.connData.BaseURL
	}
	return s.h.selfhostedBase
}

// selfhostedResult is the verbose_json shape this session reads.
type selfhostedResult struct {
	Words []struct {
		Word  string  `json:"word"`
		Start float64 `json:"start"`
		End   float64 `json:"end"`
	} `json:"words"`
}

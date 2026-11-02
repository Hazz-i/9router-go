package media

import (
	"bytes"
	json "encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"9router/proxy/internal/handlers/chat"
	"9router/proxy/internal/handlers/shared"
	"9router/proxy/internal/handlerutil"
	"9router/proxy/internal/log"
	"9router/proxy/internal/models"
	"9router/proxy/internal/providers"
)

// ElevenLabs TTS. The vendor does not speak the OpenAI /v1/audio/speech shape:
// the voice id is a path segment, the model rides as `model_id`, the payload
// field is `text`, auth is `xi-api-key`, and the render format is a query
// parameter. Forwarding the OpenAI body to cfg.TTSURL therefore produced
// `POST /v1/text-to-speech` with no voice and every call 404'd.
//
// Rotation matters more here than anywhere else in the media lanes: free
// ElevenLabs accounts carry a monthly character quota, so a single pinned
// account runs dry while five healthy ones sit idle. The loop below walks the
// provider's connections, locks whichever one the vendor rejected, and lets the
// configured round-robin strategy pick the first account of the next request.

// elevenLabsDefaultVoiceID is Sarah, a premade voice available on the free
// plan. Callers that send the OpenAI placeholder ("alloy") or nothing at all
// get a real voice instead of a 400 from the vendor.
const elevenLabsDefaultVoiceID = "EXAVITQu4vr4xnSDxMaL"

// elevenLabsOutputFormats maps the OpenAI response_format onto the vendor's
// output_format query values.
var elevenLabsOutputFormats = map[string]string{
	"mp3":  "mp3_44100_128",
	"opus": "opus_48000_128",
	"aac":  "mp3_44100_128", // no AAC render; mp3 is the closest served format
	"flac": "mp3_44100_128", // free plan has no lossless render
	"wav":  "pcm_44100",
	"pcm":  "pcm_44100",
}

type elevenLabsTTSRequest struct {
	Model          string
	Input          string
	Voice          string
	ResponseFormat string
	Speed          *float64
	VoiceSettings  map[string]any
	LanguageCode   string
	Seed           *int
	PreviousText   string
	NextText       string
	Normalization  string
}

// parseElevenLabsTTSRequest accepts both the OpenAI body and the vendor's own
// field names, so a caller written against api.elevenlabs.io works unchanged
// through the gateway.
func parseElevenLabsTTSRequest(body []byte, modelInfo *chat.ModelInfo) (*elevenLabsTTSRequest, error) {
	var raw struct {
		Model          string         `json:"model"`
		Input          string         `json:"input"`
		Text           string         `json:"text"`
		Voice          string         `json:"voice"`
		VoiceID        string         `json:"voice_id"`
		ResponseFormat string         `json:"response_format"`
		Speed          *float64       `json:"speed"`
		VoiceSettings  map[string]any `json:"voice_settings"`
		LanguageCode   string         `json:"language_code"`
		Seed           *int           `json:"seed"`
		PreviousText   string         `json:"previous_text"`
		NextText       string         `json:"next_text"`
		Normalization  string         `json:"apply_text_normalization"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("invalid JSON body")
	}

	req := &elevenLabsTTSRequest{
		Input:          strings.TrimSpace(raw.Input),
		Voice:          strings.TrimSpace(raw.Voice),
		ResponseFormat: strings.ToLower(strings.TrimSpace(raw.ResponseFormat)),
		Speed:          raw.Speed,
		VoiceSettings:  raw.VoiceSettings,
		LanguageCode:   strings.TrimSpace(raw.LanguageCode),
		Seed:           raw.Seed,
		PreviousText:   raw.PreviousText,
		NextText:       raw.NextText,
		Normalization:  strings.TrimSpace(raw.Normalization),
	}
	if req.Input == "" {
		req.Input = strings.TrimSpace(raw.Text)
	}
	if req.Input == "" {
		return nil, fmt.Errorf("Missing required field: input")
	}
	if req.Voice == "" {
		req.Voice = strings.TrimSpace(raw.VoiceID)
	}
	switch req.Voice {
	case "", "alloy", "default", "elevenlabs":
		req.Voice = elevenLabsDefaultVoiceID
	}

	// The vendor model id is what upstream knows; 9router's "el/…" spelling is
	// stripped during model resolution.
	req.Model = modelInfo.Model
	if req.Model == "" {
		req.Model = strings.TrimPrefix(raw.Model, "el/")
		req.Model = strings.TrimPrefix(req.Model, "elevenlabs/")
	}
	if req.Model == "" {
		req.Model = "eleven_multilingual_v2"
	}
	return req, nil
}

// elevenLabsTTSURL builds the vendor endpoint. The voice id is a path segment,
// which is exactly what the generic media funnel dropped.
func elevenLabsTTSURL(chatH *chat.ChatHandler, connData *chat.ConnectionData, voiceID string) string {
	base := ""
	if connData != nil && strings.TrimSpace(connData.BaseURL) != "" {
		base = strings.TrimRight(strings.TrimSpace(connData.BaseURL), "/")
	} else if cfg, err := chatH.GetProviderConfig("elevenlabs", connData); err == nil && cfg != nil {
		base = strings.TrimRight(cfg.BaseURL, "/")
	}
	if base == "" {
		base = "https://api.elevenlabs.io"
	}
	// A connection that already names the full text-to-speech path keeps it.
	if strings.Contains(base, "/text-to-speech") {
		return strings.TrimRight(base, "/") + "/" + url.PathEscape(voiceID)
	}
	base = strings.TrimSuffix(base, "/v1")
	return base + "/v1/text-to-speech/" + url.PathEscape(voiceID)
}

// elevenLabsTTSBody renders the vendor payload. Unknown OpenAI fields are not
// copied: ElevenLabs rejects a body it does not recognise.
func elevenLabsTTSBody(req *elevenLabsTTSRequest) ([]byte, error) {
	settings := map[string]any{}
	for k, v := range req.VoiceSettings {
		settings[k] = v
	}
	if req.Speed != nil {
		settings["speed"] = *req.Speed
	}

	payload := map[string]any{
		"text":     req.Input,
		"model_id": req.Model,
	}
	if len(settings) > 0 {
		payload["voice_settings"] = settings
	}
	if req.LanguageCode != "" {
		payload["language_code"] = req.LanguageCode
	}
	if req.Seed != nil {
		payload["seed"] = *req.Seed
	}
	if req.PreviousText != "" {
		payload["previous_text"] = req.PreviousText
	}
	if req.NextText != "" {
		payload["next_text"] = req.NextText
	}
	if req.Normalization != "" {
		payload["apply_text_normalization"] = req.Normalization
	}
	return json.Marshal(payload)
}

// forwardElevenLabsTTS rotates through the provider's connections until one
// renders the audio, locking the accounts the vendor refuses.
func (h *MediaHandler) forwardElevenLabsTTS(w http.ResponseWriter, r *http.Request, body []byte, modelInfo *chat.ModelInfo) {
	req, err := parseElevenLabsTTSRequest(body, modelInfo)
	if err != nil {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	upstreamBody, err := elevenLabsTTSBody(req)
	if err != nil {
		handlerutil.WriteJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Upstream parity (sttCore / tts.js credential loop): a pinned connection is
	// tried once and its error surfaces; otherwise every eligible account gets
	// one attempt in rotation order.
	pinned := modelInfo.ConnectionID
	if pinned == "" {
		pinned = imagePinnedConnectionID(r)
	}
	usePinned := pinned != ""
	excludeIDs := []string{}

	var lastStatus int
	var lastMsg string
	for {
		conn, connData, connErr := h.ChatH.GetBestConnectionWithContext(r.Context(), "elevenlabs", pinned, excludeIDs, req.Model)
		if connErr != nil || conn == nil || connData == nil {
			if lastMsg != "" {
				handlerutil.WriteJSONError(w, lastStatus, lastMsg)
				return
			}
			handlerutil.WriteJSONError(w, http.StatusServiceUnavailable, mediaConnectionError(connErr, "elevenlabs"))
			return
		}
		status, msg, done := h.tryElevenLabsTTSConn(w, r, req, upstreamBody, conn, connData)
		if done {
			return
		}
		lastStatus, lastMsg = status, msg
		if usePinned {
			handlerutil.WriteJSONError(w, status, msg)
			return
		}
		excludeIDs = append(excludeIDs, conn.ID)
	}
}

func (h *MediaHandler) tryElevenLabsTTSConn(w http.ResponseWriter, r *http.Request, req *elevenLabsTTSRequest, upstreamBody []byte, conn *models.ProviderConnection, connData *chat.ConnectionData) (int, string, bool) {
	apiKey := chat.ExtractAPIKey(connData)
	if apiKey == "" {
		return http.StatusUnauthorized, "no API key found for elevenlabs connection " + conn.ID, false
	}

	providerCfg, err := h.ChatH.GetProviderConfig("elevenlabs", connData)
	if err != nil || providerCfg == nil {
		return http.StatusInternalServerError, "get elevenlabs config: " + errText(err), false
	}

	targetURL := elevenLabsTTSURL(h.ChatH, connData, req.Voice)
	if format := elevenLabsOutputFormats[req.ResponseFormat]; format != "" {
		targetURL += "?output_format=" + url.QueryEscape(format)
	}

	httpReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, targetURL, bytes.NewReader(upstreamBody))
	if err != nil {
		return http.StatusInternalServerError, "failed to create elevenlabs tts request", false
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "audio/mpeg")
	authHeader := providerCfg.AuthHeader
	if authHeader == "" {
		authHeader = "xi-api-key"
	}
	handlerutil.SetAuthHeader(httpReq, apiKey, authHeader, providerCfg.AuthScheme)
	for k, v := range providerCfg.StaticHeaders {
		httpReq.Header.Set(k, v)
	}

	client, clientErr := h.ChatH.GetClientForConnection(connData)
	if clientErr != nil {
		return http.StatusBadGateway, clientErr.Error(), false
	}

	start := time.Now()
	resp, err := client.Do(httpReq)
	if err != nil {
		log.Warn("media", "elevenlabs tts upstream failed", "conn", conn.ID, "error", err)
		return http.StatusBadGateway, "elevenlabs tts request failed: " + err.Error(), true
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		errText := strings.TrimSpace(string(errBytes))
		if errText == "" {
			errText = resp.Status
		}
		log.Warn("media", "elevenlabs tts upstream error", "conn", conn.ID, "status", resp.StatusCode, "body", errText)
		if h.Repo != nil && !handlerutil.IsProbeContext(r.Context()) {
			backoff := h.Repo.GetConnectionBackoffLevel(conn.ID)
			if classification := providers.ClassifyErrorForProvider(resp.StatusCode, errText, backoff, "elevenlabs"); classification.ShouldFallback {
				cooldownSec := max(classification.CooldownMs/1000, 1)
				_ = h.Repo.LockConnectionModel(conn.ID, req.Model, cooldownSec, classification.NewBackoffLevel)
			}
		}
		return resp.StatusCode, errText, false
	}

	audio, err := io.ReadAll(resp.Body)
	if err != nil {
		return http.StatusBadGateway, "failed to read elevenlabs audio response", true
	}

	if h.Repo != nil {
		_ = h.Repo.UpdateConnectionLastUsed(conn.ID)
		if !handlerutil.IsProbeContext(r.Context()) {
			_ = h.Repo.UnlockConnectionModel(conn.ID, req.Model)
		}
	}

	h.writeTTSResponse(w, r, req.ResponseFormat, audio, "audio/mpeg", "mp3")

	h.trackTTSUsage("elevenlabs", req.Model)
	if h.ChatH != nil {
		h.ChatH.LogUsage(r.Context(), &shared.UsageLogInfo{
			Provider:     "elevenlabs",
			Model:        req.Model,
			ConnectionID: conn.ID,
			ConnName:     connDisplayName(conn),
			APIKey:       apiKey,
			Endpoint:     "/v1/audio/speech",
		}, nil, time.Since(start).Milliseconds(), nil, nil)
	}
	return 0, "", true
}

func errText(err error) string {
	if err == nil {
		return "unknown error"
	}
	return err.Error()
}

func connDisplayName(conn *models.ProviderConnection) string {
	if conn == nil || conn.Name == nil {
		return ""
	}
	return *conn.Name
}

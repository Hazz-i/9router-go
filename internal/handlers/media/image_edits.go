package media

import (
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"9router/proxy/internal/constants"
	"9router/proxy/internal/handlers/chat"
	"9router/proxy/internal/handlers/shared"
	"9router/proxy/internal/handlerutil"
	"9router/proxy/internal/log"
	"9router/proxy/internal/providers"
)

// imageEditsMaxUpload caps the total bytes an /images/edits upload may carry
// across all parts (the attached image, the mask and the fields). It sits under
// the middleware's MediaMaxBodySize ceiling so the handler's own 413 is the one
// the caller sees.
const imageEditsMaxUpload = 20 << 20 // 20MB

// defaultImageEditModel matches the gpt-image editing family a caller that
// omits model should get, rather than a generation-only id like dall-e-3.
const defaultImageEditModel = "gpt-image-2"

var errEditsTooLarge = errors.New("image edit upload too large")

// HandleImageEdits handles /v1/images/edits (OpenAI image edit): the caller
// attaches an image and an instruction ("make the background blue") and the
// request is re-spooled multipart and streamed to the provider's
// /v1/images/edits. Any OpenAI-compatible image provider serves it — kenari is
// the motivating one, but the lane is provider-agnostic by design.
func (h *MediaHandler) HandleImageEdits(w http.ResponseWriter, r *http.Request) {
	mr, err := r.MultipartReader()
	if err != nil {
		handlerutil.WriteJSONError(w, http.StatusBadRequest,
			"expected multipart/form-data with an image file part")
		return
	}

	parts, err := spoolMultipartParts(mr)
	defer cleanupSpooled(parts)
	if err != nil {
		if errors.Is(err, errEditsTooLarge) {
			handlerutil.WriteJSONError(w, http.StatusRequestEntityTooLarge,
				"image edit upload too large: this endpoint accepts up to 20MB total")
			return
		}
		handlerutil.WriteJSONError(w, http.StatusBadRequest,
			fmt.Sprintf("malformed multipart body: %v", err))
		return
	}
	if !hasImageFile(parts) {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "missing image file part")
		return
	}

	model := fieldValue(parts, "model")
	if model == "" {
		model = defaultImageEditModel
	}
	if !h.ChatH.EnforceModelAccess(w, r, model) {
		return
	}
	modelInfo, err := h.ChatH.ResolveModel(model)
	if err != nil {
		log.Warn("media", "resolve model failed", "endpoint", "/v1/images/edits", "model", model, "error", err)
		handlerutil.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	preferredConnID := r.Header.Get("x-connection-id")
	if preferredConnID == "" {
		preferredConnID = r.Header.Get("x-provider-connection-id")
	}
	if preferredConnID == "" {
		preferredConnID = modelInfo.ConnectionID
	}
	conn, connData, err := h.ChatH.GetBestConnectionWithContext(r.Context(), modelInfo.Provider, preferredConnID, nil, modelInfo.Model)
	if err != nil || connData == nil {
		// Same no-auth fallback the generations path uses, so a provider that
		// needs no credential still reaches its lane.
		if cfg, ok := providers.KnownProviders[modelInfo.Provider]; ok && (cfg.NoAuth || cfg.DefaultAPIKey != "") {
			apiKey := cfg.DefaultAPIKey
			if apiKey == "" {
				apiKey = "public"
			}
			connData = &chat.ConnectionData{APIKey: apiKey, ProxyPoolID: h.ChatH.ResolveProviderProxyPoolID(modelInfo.Provider)}
		} else {
			log.Warn("media", "no connection", "endpoint", "/v1/images/edits", "provider", modelInfo.Provider, "model", modelInfo.Model, "error", err)
			handlerutil.WriteJSONError(w, http.StatusNotFound, mediaConnectionError(err, modelInfo.Provider))
			return
		}
	}
	connID := ""
	if conn != nil {
		connID = conn.ID
	}

	providerCfg, err := h.ChatH.GetProviderConfig(modelInfo.Provider, connData)
	if err != nil {
		handlerutil.WriteJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	apiKey := chat.ExtractAPIKey(connData)
	if apiKey == "" {
		if providerCfg != nil && providerCfg.DefaultAPIKey != "" {
			apiKey = providerCfg.DefaultAPIKey
		} else {
			handlerutil.WriteJSONError(w, http.StatusUnauthorized, "no API key found")
			return
		}
	}
	h.forwardImageEdits(w, r, parts, modelInfo, connID, connData, providerCfg, apiKey)
}

// forwardImageEdits rebuilds the spooled upload into the upstream request body
// and streams it: a goroutine writes the multipart parts into an io.Pipe whose
// reader is the upstream request body, so file bytes never buffer whole. The
// model part is rewritten to the resolved upstream id; every other part is
// replayed with its original headers verbatim.
func (h *MediaHandler) forwardImageEdits(w http.ResponseWriter, r *http.Request, parts []spooledPart, modelInfo *shared.ModelInfo, connID string, connData *chat.ConnectionData, providerCfg *providers.ProviderConfig, apiKey string) {
	baseURL := strings.TrimRight(providerCfg.BaseURL, "/")
	if connData != nil && connData.BaseURL != "" {
		baseURL = strings.TrimRight(connData.BaseURL, "/")
	}
	baseURL = strings.TrimSuffix(baseURL, "/chat/completions")
	baseURL = strings.TrimSuffix(baseURL, "/messages")
	// Same /v1 normalisation the generations path uses (media.go).
	targetURL := strings.TrimSuffix(baseURL, "/v1") + "/v1/images/edits"

	client, clientErr := h.ChatH.GetClientForConnection(connData)
	if clientErr != nil {
		handlerutil.WriteJSONError(w, http.StatusBadGateway, clientErr.Error())
		return
	}

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	// Pin the boundary before the writer goroutine starts: FormDataContentType
	// lazily generates it, and only the goroutine writes parts afterwards.
	contentType := mw.FormDataContentType()
	done := make(chan struct{})
	go func() {
		defer close(done)
		pw.CloseWithError(writeSpooledParts(mw, parts, modelInfo.Model))
	}()

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, targetURL, pr)
	if err != nil {
		pr.Close()
		<-done
		handlerutil.WriteJSONError(w, http.StatusInternalServerError, "failed to create request")
		return
	}
	req.Header.Set(constants.HeaderContentType, contentType)
	handlerutil.SetAuthHeader(req, apiKey, providerCfg.AuthHeader, providerCfg.AuthScheme)
	for k, v := range providerCfg.StaticHeaders {
		if req.Header.Get(k) == "" {
			req.Header.Set(k, v)
		}
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		// Unblock the writer goroutine: the transport is no longer reading.
		pr.Close()
		<-done
		log.Error("media", "upstream request failed", "endpoint", "/v1/images/edits", "provider", modelInfo.Provider, "model", modelInfo.Model, "error", err)
		handlerutil.WriteJSONError(w, http.StatusBadGateway, "upstream request failed")
		return
	}
	defer resp.Body.Close()

	copyUpstreamHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
	// The spooled temp files back the body the transport read; only once the
	// writer goroutine finished may they be removed.
	<-done

	if resp.StatusCode == http.StatusOK {
		if connID != "" {
			h.Repo.UpdateConnectionLastUsed(connID)
		}
		latencyMs := time.Since(start).Milliseconds()
		h.ChatH.LogUsage(r.Context(), &shared.UsageLogInfo{
			Provider:     modelInfo.Provider,
			Model:        modelInfo.Model,
			ConnectionID: connID,
			APIKey:       apiKey,
			Endpoint:     "/v1/images/edits",
		}, nil, latencyMs, nil, nil)
	}
}

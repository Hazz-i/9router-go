package realtime

import "encoding/json/v2"

// Client-facing event builders for /v1/realtime. This file is the Go port of
// upstream's openaiEvents.js: the browser contract is the OpenAI Realtime
// protocol and every adapter maps its upstream events through these builders —
// never inventing new wire types.

func sendClientError(c *adapterCtx, message string) {
	c.sendClient(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "upstream_error", "message": message},
	})
}

// sendSessionCreated sends the full session document. voice is omitted when
// empty (Deepgram carries no voice).
func sendSessionCreated(c *adapterCtx, sessionID, model, voice string) {
	session := map[string]any{
		"id":                   sessionID,
		"object":               "realtime.session",
		"model":                model,
		"modalities":           []string{"audio", "text"},
		"turn_detection":       map[string]any{"type": "server_vad"},
		"input_audio_format":   "pcm16",
		"output_audio_format":  "pcm16",
	}
	if voice != "" {
		session["voice"] = voice
	}
	c.sendClient(map[string]any{"type": "session.created", "session": session})
}

func sendSessionUpdated(c *adapterCtx, sessionID, model, voice string) {
	session := map[string]any{
		"id":     sessionID,
		"object": "realtime.session",
		"model":  model,
	}
	if voice != "" {
		session["voice"] = voice
	}
	c.sendClient(map[string]any{"type": "session.updated", "session": session})
}

func sendAudioDelta(c *adapterCtx, responseID, itemID, base64 string) {
	c.sendClient(map[string]any{
		"type":          "response.audio.delta",
		"response_id":   responseID,
		"item_id":       itemID,
		"output_index":  0,
		"content_index": 0,
		"delta":         base64,
	})
}

func sendTranscriptDelta(c *adapterCtx, responseID, itemID, text string) {
	c.sendClient(map[string]any{
		"type":          "response.audio_transcript.delta",
		"response_id":   responseID,
		"item_id":       itemID,
		"output_index":  0,
		"content_index": 0,
		"delta":         text,
	})
}

func sendSpeechStarted(c *adapterCtx, itemID string, audioStartMs int64) {
	event := map[string]any{"type": "input_audio_buffer.speech_started", "item_id": itemID}
	if audioStartMs >= 0 {
		event["audio_start_ms"] = audioStartMs
	}
	c.sendClient(event)
}

func sendResponseCancelled(c *adapterCtx, responseID string) {
	c.sendClient(map[string]any{"type": "response.cancelled", "response_id": responseID})
}

// sendTurnComplete sends response.audio.done followed immediately by
// response.done, then rotates ids at the call site.
func sendTurnComplete(c *adapterCtx, responseID, itemID string) {
	c.sendClient(map[string]any{
		"type":        "response.audio.done",
		"response_id": responseID,
		"item_id":     itemID,
	})
	c.sendClient(map[string]any{
		"type": "response.done",
		"response": map[string]any{
			"id":     responseID,
			"object": "realtime.response",
			"status": "completed",
		},
	})
}

// marshalEvent encodes one client frame. The write itself never fails the
// session: a marshalling error is logged upstream in real JS via JSON.stringify
// (which cannot fail), so a deterministic failure here is a bug, not input.
func marshalEvent(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"type":"error","error":{"type":"server_error","message":"internal marshal failure"}}`)
	}
	return b
}

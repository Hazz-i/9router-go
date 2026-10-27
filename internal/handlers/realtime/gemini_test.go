package realtime

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// startGeminiSession builds a repo+gateway wired to a gated fake Gemini Live
// upstream, seeds one gemini connection whose providerSpecificData.baseUrl
// points at the fake (the per-connection override), dials the client, and
// gates the fake handshake on fake.open(). The fake records EVERY upstream
// frame until the socket closes; `drive` runs inline after the first `read`
// frames are recorded (drive only writes upstream, so the single-reader rule
// holds). Recording must never stop at the budget: the silence heartbeat
// alone would otherwise eat it before the client can send audio.
func startGeminiSession(t *testing.T, read int, drive func(conn *websocket.Conn)) (*fakeUpstream, *websocket.Conn) {
	t.Helper()
	var fake *fakeUpstream
	fake = newFakeUpstream(t, func(t *testing.T, conn *websocket.Conn) {
		driven := false
		for i := 0; ; i++ {
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			fake.recordRaw(data)
			if !driven && i+1 >= read {
				driven = true
				if drive != nil {
					drive(conn)
				}
			}
		}
	})
	database, repo := newTestRepo(t)
	seedConnection(t, database, "gemini", "gk-1", map[string]any{"baseUrl": fake.URL})
	gateway := newGateway(t, repo, map[string]string{"gemini": "wss://real-gemini.invalid/ws"})
	client := dialWS(t, gateway.URL, "", nil)
	return fake, client
}

// TestGeminiWire_SetupTranscriptionFieldsAreGenerationConfigSiblings locks the
// setup-frame shape: transcription flags are TOP-LEVEL siblings of
// generationConfig, never nested inside it.
func TestGeminiWire_SetupTranscriptionFieldsAreGenerationConfigSiblings(t *testing.T) {
	drive := func(conn *websocket.Conn) {
		writeJSON(t, conn, map[string]any{"setupComplete": map[string]any{}})
	}
	fake, client := startGeminiSession(t, 1, drive)
	fake.open()
	waitForEvent(t, client, "session.created", 3*time.Second)
	waitForFrameCount(t, fake, 1)

	s := unmarshalFrame(t, []byte(fake.frame(0)))["setup"].(map[string]any)
	if _, ok := s["inputAudioTranscription"].(map[string]any); !ok {
		t.Errorf("setup.inputAudioTranscription must be a top-level {} sibling, got %v", s["inputAudioTranscription"])
	}
	if _, ok := s["outputAudioTranscription"].(map[string]any); !ok {
		t.Errorf("setup.outputAudioTranscription must be a top-level {} sibling, got %v", s["outputAudioTranscription"])
	}
	gc := s["generationConfig"].(map[string]any)
	for _, nested := range []string{"inputAudioTranscription", "outputAudioTranscription"} {
		if _, present := gc[nested]; present {
			t.Errorf("generationConfig must NOT carry %s — it is not nested", nested)
		}
	}
}

// TestGeminiWire_AppendAndBinaryFramesRideRealtimeInputAudio locks the audio
// wire: JSON append events and raw binary client frames both map to the exact
// {realtimeInput:{audio:{mimeType,data}}} shape, and no deprecated
// "mediaChunks" key may ever appear in an upstream frame.
func TestGeminiWire_AppendAndBinaryFramesRideRealtimeInputAudio(t *testing.T) {
	drive := func(conn *websocket.Conn) {
		writeJSON(t, conn, map[string]any{"setupComplete": map[string]any{}})
	}
	// Stage BOTH audio paths only after the upstream confirmed setup, so the
	// frame order is deterministic: setup precedes the first audio chunk.
	fake, client := startGeminiSession(t, 6, drive)
	fake.open()
	waitForEvent(t, client, "session.created", 3*time.Second)
	writeJSON(t, client, map[string]any{"type": "input_audio_buffer.append", "audio": "AAAA"})
	if err := client.WriteMessage(websocket.BinaryMessage, []byte{1, 2, 3, 4}); err != nil {
		t.Fatalf("client binary: %v", err)
	}

	// Scan the recorded frames: the append and binary paths must both land as
	// the byte-exact realtimeInput.audio shape, setup must precede both, and
	// no deprecated "mediaChunks" key may ever appear.
	wantAppend := map[string]any{
		"realtimeInput": map[string]any{
			"audio": map[string]any{"mimeType": "audio/pcm;rate=16000", "data": "AAAA"},
		},
	}
	wantBinary := map[string]any{
		"realtimeInput": map[string]any{
			"audio": map[string]any{"mimeType": "audio/pcm;rate=16000", "data": "AQIDBA=="},
		},
	}
	sawAppend, sawBinary, setupIdx, appendIdx, binaryIdx := false, false, -1, -1, -1
	deadline := time.Now().Add(3 * time.Second)
	for (!sawAppend || !sawBinary) && time.Now().Before(deadline) {
		for i := 0; i < fake.frameCount(); i++ {
			frame := unmarshalFrame(t, []byte(fake.frame(i)))
			if _, isSetup := frame["setup"]; isSetup && setupIdx == -1 {
				setupIdx = i
			}
			if appendIdx == -1 && deepEqual(frame, wantAppend) == "" {
				sawAppend, appendIdx = true, i
			}
			if binaryIdx == -1 && deepEqual(frame, wantBinary) == "" {
				sawBinary, binaryIdx = true, i
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !sawAppend || !sawBinary {
		t.Fatalf("audio frames did not both land as realtimeInput.audio: append=%v binary=%v", sawAppend, sawBinary)
	}
	if setupIdx == -1 || setupIdx > appendIdx || setupIdx > binaryIdx {
		t.Errorf("setup (index %d) must be out with the first audio (append %d, binary %d)", setupIdx, appendIdx, binaryIdx)
	}
	for i := 0; i < fake.frameCount(); i++ {
		if strings.Contains(fake.frame(i), "mediaChunks") {
			t.Errorf("the literal mediaChunks must never appear in an upstream frame:\n%s", fake.frame(i))
		}
	}
}

// TestGeminiWire_SilenceHeartbeatFeedsServerVAD locks the mandatory heartbeat:
// after setup every subsequent upstream frame is 2048 zero bytes of PCM16 @16k
// riding realtimeInput.audio, and at least two frames arrive promptly.
func TestGeminiWire_SilenceHeartbeatFeedsServerVAD(t *testing.T) {
	fake, _ := startGeminiSession(t, 4, nil)
	fake.open()
	waitForFrameCount(t, fake, 4) // setup + 3 heartbeats

	want := map[string]any{
		"realtimeInput": map[string]any{
			"audio": map[string]any{
				"mimeType": "audio/pcm;rate=16000",
				"data":     base64.StdEncoding.EncodeToString(make([]byte, 2048)),
			},
		},
	}
	setup := unmarshalFrame(t, []byte(fake.frame(0)))
	if _, ok := setup["setup"]; !ok {
		t.Fatalf("expected the setup frame first, got %v", setup)
	}
	for i := 1; i <= 3; i++ {
		if diff := deepEqual(unmarshalFrame(t, []byte(fake.frame(i))), want); diff != "" {
			t.Errorf("upstream frame %d = %s, want the 2048-zero-byte silence frame (%s)", i, fake.frame(i), diff)
		}
	}
}

// TestGeminiWire_UpstreamEventMappingOrderWithIDRotation drives a full turn and
// an interruption against the fake and asserts the exact gateway→client event
// order. Upstream rotates ids after every turnComplete, so the cancel carries
// the NEW in-flight id — which must differ from the first turn's id.
func TestGeminiWire_UpstreamEventMappingOrderWithIDRotation(t *testing.T) {
	drive := func(conn *websocket.Conn) {
		writeJSON(t, conn, map[string]any{"setupComplete": map[string]any{}})
		writeJSON(t, conn, map[string]any{"serverContent": map[string]any{
			"outputTranscription": map[string]any{"text": "hello"},
		}})
		writeJSON(t, conn, map[string]any{"serverContent": map[string]any{
			"modelTurn": map[string]any{"parts": []any{
				map[string]any{"inlineData": map[string]any{"data": "AQ=="}, "text": "hi"},
			}},
		}})
		writeJSON(t, conn, map[string]any{"serverContent": map[string]any{"turnComplete": true}})
		writeJSON(t, conn, map[string]any{"serverContent": map[string]any{"interrupted": true}})
		writeJSON(t, conn, map[string]any{"serverContent": map[string]any{"turnComplete": true}})
	}
	fake, client := startGeminiSession(t, 1, drive)
	fake.open()

	wantOrder := []string{
		"session.created", "session.updated",
		"response.audio_transcript.delta",
		"response.audio.delta", "response.audio_transcript.delta",
		"response.audio.done", "response.done",
		"input_audio_buffer.speech_started", "response.cancelled",
		"response.audio.done", "response.done",
	}
	var gotOrder []string
	var firstTurnID, cancelID, secondTurnID string
	for len(gotOrder) < len(wantOrder) {
		ev := readJSON(t, client, 3*time.Second)
		kind := eventType(ev)
		gotOrder = append(gotOrder, kind)
		switch kind {
		case "response.done":
			id := ev["response"].(map[string]any)["id"].(string)
			if firstTurnID == "" {
				firstTurnID = id
			} else {
				secondTurnID = id
			}
		case "response.cancelled":
			cancelID = ev["response_id"].(string)
		}
	}
	if strings.Join(gotOrder, ",") != strings.Join(wantOrder, ",") {
		t.Fatalf("event order = %v, want %v", gotOrder, wantOrder)
	}
	if cancelID == "" || cancelID == firstTurnID {
		t.Errorf("response.cancelled carried %q — it must carry the post-rotation in-flight id, not the first turn's %q", cancelID, firstTurnID)
	}
	if secondTurnID == "" || secondTurnID == cancelID {
		t.Errorf("response ids must rotate again after the cancel: cancel %q, second turn %q", cancelID, secondTurnID)
	}
}

// updateObserver wraps an Adapter and signals handled after the client read
// loop has finished handling a session.update: the happens-before edge that
// lets the merge test release the fake's handshake gate only once
// handleSessionUpdate's merge (its mu critical section) is complete. Nothing
// observable flows to the client or the fake before the upstream opens (the
// merge path is silent — sendSetup no-ops while up is closed), so "the
// gateway processed the update" would otherwise be hoped for, not enforced.
type updateObserver struct {
	Adapter
	handled chan struct{}
}

func (o *updateObserver) HandleClientEvent(event map[string]any) {
	o.Adapter.HandleClientEvent(event)
	if eventType(event) == "session.update" {
		select {
		case o.handled <- struct{}{}:
		default:
		}
	}
}

// TestGeminiWire_SessionUpdateBeforeSetupIsMergedIntoSetupFrame: a
// session.update arriving before the upstream dial completes is applied into
// the (still un-sent) setup frame — voice and instructions both.
func TestGeminiWire_SessionUpdateBeforeSetupIsMergedIntoSetupFrame(t *testing.T) {
	// Wrap the gemini factory FIRST: its restore is registered earliest and
	// therefore runs LAST in the LIFO cleanup chain, after waitSessions has
	// drained every session goroutine — so the map write can never race the
	// createRealtimeAdapter read on a live handler goroutine.
	handled := make(chan struct{}, 1)
	orig := realtimeAdapters["gemini"]
	realtimeAdapters["gemini"] = func(c *adapterCtx) (Adapter, error) {
		ad, err := orig(c)
		if err != nil {
			return nil, err
		}
		return &updateObserver{Adapter: ad, handled: handled}, nil
	}
	t.Cleanup(func() { realtimeAdapters["gemini"] = orig })

	drive := func(conn *websocket.Conn) {
		writeJSON(t, conn, map[string]any{"setupComplete": map[string]any{}})
	}
	var fake *fakeUpstream
	fake = newFakeUpstream(t, func(t *testing.T, conn *websocket.Conn) {
		for i := 0; i < 2; i++ {
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			fake.recordRaw(data)
		}
		drive(conn)
	})
	database, repo := newTestRepo(t)
	seedConnection(t, database, "gemini", "gk-1", map[string]any{"baseUrl": fake.URL})
	gateway := newGateway(t, repo, map[string]string{"gemini": "wss://real-gemini.invalid/ws"})

	// Stage the client frame BEFORE the upstream handshake completes, and
	// release the gate only once the gateway has actually MERGED it — the
	// dial goroutine's sendSetup then happens entirely after the merge.
	client := dialWS(t, gateway.URL, "", nil)
	writeJSON(t, client, map[string]any{
		"type":    "session.update",
		"session": map[string]any{"voice": "Kore", "instructions": "Custom instruction"},
	})
	waitFor(t, 3*time.Second, "session.update handled by the client read loop", func() bool {
		select {
		case <-handled:
			return true
		default:
			return false
		}
	})
	fake.open()
	waitForEvent(t, client, "session.created", 3*time.Second)
	waitForFrameCount(t, fake, 2)

	// frame 0 is the heartbeating silence feed only if setup already went out;
	// with the staged session.update the setup is frame 0 or 1 — find it.
	var setupFrame string
	for i := 0; i < 2; i++ {
		if _, ok := unmarshalFrame(t, []byte(fake.frame(i)))["setup"]; ok {
			setupFrame = fake.frame(i)
			break
		}
	}
	if setupFrame == "" {
		t.Fatalf("no setup frame recorded: %s / %s", fake.frame(0), fake.frame(1))
	}
	s := unmarshalFrame(t, []byte(setupFrame))["setup"].(map[string]any)
	gc := s["generationConfig"].(map[string]any)
	prebuilt := gc["speechConfig"].(map[string]any)["voiceConfig"].(map[string]any)["prebuiltVoiceConfig"].(map[string]any)
	if prebuilt["voiceName"] != "Kore" {
		t.Errorf("setup voiceName = %v, want the session.update override Kore", prebuilt["voiceName"])
	}
	parts := s["systemInstruction"].(map[string]any)["parts"].([]any)
	if text := parts[0].(map[string]any)["text"]; text != "Custom instruction" {
		t.Errorf("setup instructions = %v, want the session.update override", text)
	}
}

// waitForFrameCount polls until the fake has recorded n frames.
func waitForFrameCount(t *testing.T, fake *fakeUpstream, n int) {
	t.Helper()
	waitFor(t, 3*time.Second, "n upstream frames", func() bool { return fake.frameCount() >= n })
}

// TestGeminiWire_UpstreamErrorFrameMapsToClientError: an upstream {error}
// frame becomes the client `error` event with type upstream_error and the
// Gemini message — never a client close (mid-session upstream errors do not
// end the session, §2.6).
func TestGeminiWire_UpstreamErrorFrameMapsToClientError(t *testing.T) {
	drive := func(conn *websocket.Conn) {
		writeJSON(t, conn, map[string]any{"setupComplete": map[string]any{}})
		writeJSON(t, conn, map[string]any{"error": map[string]any{
			"message": "model is overloaded",
		}})
	}
	fake, client := startGeminiSession(t, 1, drive)
	fake.open()
	waitForEvent(t, client, "session.created", 3*time.Second)

	ev := waitForEvent(t, client, "error", 3*time.Second)
	errBody := ev["error"].(map[string]any)
	if errBody["type"] != "upstream_error" || errBody["message"] != "model is overloaded" {
		t.Errorf("error event = %v, want upstream_error 'model is overloaded'", errBody)
	}
}

// TestGeminiWire_UpdateBeforeSetupAckIsNotEchoedEarly pins the ordering the
// two setup frames rely on: between the setup frame going upstream and the
// upstream's setupComplete echo, a session.update is stored but NOT echoed.
// Echoing there wrote the update's session.updated between session.created and
// the setup's own session.updated, so the stale default voice landed last and
// reverted the client's choice (the -race race-detector job caught it).
func TestGeminiWire_UpdateBeforeSetupAckIsNotEchoedEarly(t *testing.T) {
	release := make(chan struct{})
	drive := func(conn *websocket.Conn) {
		<-release
		writeJSON(t, conn, map[string]any{"setupComplete": map[string]any{}})
	}
	fake, client := startGeminiSession(t, 1, drive)
	fake.open()
	waitFor(t, 3*time.Second, "setup frame upstream", func() bool { return fake.frameCount() >= 1 })

	writeJSON(t, client, map[string]any{
		"type":    "session.update",
		"session": map[string]any{"voice": "Kore"},
	})
	// Let the client read loop handle it while the ack is still outstanding.
	time.Sleep(150 * time.Millisecond)
	close(release)

	// Strict order: the ack's session.created must be the first frame the
	// client sees. An early echo would have arrived before it.
	first := readJSON(t, client, 3*time.Second)
	if got := eventType(first); got != "session.created" {
		t.Fatalf("first frame after the update = %q, want session.created", got)
	}
	ev := waitForEvent(t, client, "session.updated", 3*time.Second)
	if voice := ev["session"].(map[string]any)["voice"]; voice != "Kore" {
		t.Errorf("setup echo voice = %v, want the update's voice Kore", voice)
	}
}

// TestGeminiWire_SessionUpdateAfterSetupEchoesOnly: once the setup frame is
// out, a session.update no longer re-sends anything upstream — it only echoes
// session.updated (§2.5).
func TestGeminiWire_SessionUpdateAfterSetupEchoesOnly(t *testing.T) {
	drive := func(conn *websocket.Conn) {
		writeJSON(t, conn, map[string]any{"setupComplete": map[string]any{}})
	}
	fake, client := startGeminiSession(t, 1, drive)
	fake.open()
	waitForEvent(t, client, "session.created", 3*time.Second)

	writeJSON(t, client, map[string]any{
		"type":    "session.update",
		"session": map[string]any{"voice": "Kore", "instructions": "Custom instruction"},
	})
	// The setupComplete echo (voice Puck) precedes the update's echo — read
	// past it and assert on the second one.
	waitForEvent(t, client, "session.updated", 3*time.Second)
	ev := waitForEvent(t, client, "session.updated", 3*time.Second)
	if voice := ev["session"].(map[string]any)["voice"]; voice != "Kore" {
		t.Errorf("session.updated voice = %v, want the update's voice Kore", voice)
	}
	// Give any illegal upstream re-send a beat to arrive, then assert none:
	// after setup the only thing that flows upstream is the silence
	// heartbeat — no second setup, no clientContent.
	time.Sleep(150 * time.Millisecond)
	for i := 0; i < fake.frameCount(); i++ {
		frame := unmarshalFrame(t, []byte(fake.frame(i)))
		if _, isSetup := frame["setup"]; isSetup && i > 0 {
			t.Errorf("a second setup frame was sent upstream after session.update")
		}
		if _, isClientContent := frame["clientContent"]; isClientContent {
			t.Errorf("session.update must not be re-sent upstream as clientContent: %s", fake.frame(i))
		}
	}
}

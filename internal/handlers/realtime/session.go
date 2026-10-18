package realtime

import (
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"sync"
	"time"

	"9router/proxy/internal/db"
	"9router/proxy/internal/handlerutil"
	"9router/proxy/internal/log"
	"github.com/gorilla/websocket"
)

const realtimeUsageEndpoint = "/v1/realtime"

// session owns one upgraded client socket: the client read pump, the shared
// usage counters the adapters write from their own goroutines, the once-only
// teardown and the usageHistory row.
type session struct {
	repo   *db.Repo
	conn   *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc
	start  time.Time

	// wg tracks every goroutine spawned for this session (upstream read
	// pumps, heartbeat/keepalive tickers, the dial goroutine). teardown joins
	// them all, so nothing outlives the repo the usage row is written to.
	wg sync.WaitGroup
	// writeMu serialises gateway→client writes: gorilla allows exactly one
	// concurrent writer and the client read loop, the upstream read pump and
	// teardown all write.
	writeMu sync.Mutex

	mu          sync.Mutex
	closed      bool // teardown ran; guards usage-once
	inputBytes  int64
	outputBytes int64
	inputRate   int
	outputRate  int

	provider   string
	model      string
	connID     string
	connAPIKey string
}

func newSession(repo *db.Repo, conn *websocket.Conn) *session {
	ctx, cancel := context.WithCancel(context.Background())
	return &session{
		repo:       repo,
		conn:       conn,
		ctx:        ctx,
		cancel:     cancel,
		start:      time.Now(),
		inputRate:  16000,
		outputRate: 24000,
	}
}

func (s *session) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// sendClient writes one text JSON frame. gorilla allows one concurrent writer,
// so every gateway→client write serialises here (audio deltas arrive from the
// upstream read pump while echoes come from the client read loop).
func (s *session) sendClient(v any) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = s.conn.WriteMessage(websocket.TextMessage, marshalEvent(v))
}

func (s *session) closeClient(code int, reason string) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	closeConn(s.conn, code, reason)
}

func (s *session) countInput(n int) {
	s.mu.Lock()
	s.inputBytes += int64(n)
	s.mu.Unlock()
}

func (s *session) countOutput(n int) {
	s.mu.Lock()
	s.outputBytes += int64(n)
	s.mu.Unlock()
}

func (s *session) setOutputRate(r int) {
	s.mu.Lock()
	s.outputRate = r
	s.mu.Unlock()
}

// readLoop pumps client frames until the client disconnects. Binary frames are
// raw PCM16 audio and behave exactly like input_audio_buffer.append.
func (s *session) readLoop(ad Adapter) {
	for {
		msgType, data, err := s.conn.ReadMessage()
		if err != nil {
			return // normal disconnect, close frame or socket error
		}
		if msgType == websocket.BinaryMessage {
			s.countInput(len(data))
			ad.HandleClientAudio(data)
			continue
		}
		var event map[string]any
		if err := json.Unmarshal(data, &event); err != nil {
			log.Warn("REALTIME_VOICE", "dropping unparseable client frame")
			continue
		}
		if event["type"] == "input_audio_buffer.append" {
			if audio, _ := event["audio"].(string); audio != "" {
				s.countInput(base64.StdEncoding.DecodedLen(len(audio)))
			}
		}
		ad.HandleClientEvent(event)
	}
}

// teardown runs exactly once per session: stops the adapter timers via ctx
// cancellation, closes the upstream with "Client closed", writes the usage row
// and then JOINS every session goroutine — the handler must not return while
// anything can still touch the repo or the client socket. It is triggered by
// the client read loop returning (client close or error) — the same moment
// upstream JS's clientWs "close"/"error" handlers fire.
func (s *session) teardown(ad Adapter) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()

	s.cancel()
	if ad != nil {
		ad.Close()
	}
	s.recordUsage()
	s.wg.Wait()
}

// recordUsage writes one usageHistory row. Token estimate: ~25 tokens per
// second of PCM audio, seconds derived from the byte counts and the
// configured sample rates (bytes = rate × 2 bytes × seconds).
func (s *session) recordUsage() {
	s.mu.Lock()
	inSeconds := float64(s.inputBytes) / float64(2*s.inputRate)
	outSeconds := float64(s.outputBytes) / float64(2*s.outputRate)
	prompt := maxInt(1, roundToInt(inSeconds*25))
	completion := maxInt(1, roundToInt(outSeconds*25))
	durationMs := time.Since(s.start).Milliseconds()
	s.mu.Unlock()

	provider, model, connID := s.provider, s.model, s.connID
	apiKey := handlerutil.MaskAPIKey(s.connAPIKey)

	tokensJSON, _ := json.Marshal(map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"cached_tokens":     0,
	})
	metaJSON, _ := json.Marshal(map[string]any{
		"durationMs": durationMs,
		"statusCode": 200,
	})
	if err := s.repo.InsertUsageHistory(provider, model, connID, apiKey,
		realtimeUsageEndpoint, prompt, completion, 0, "success", prompt+completion,
		string(metaJSON), string(tokensJSON)); err != nil {
		log.Error("REALTIME_VOICE", "failed to record session usage", "error", err)
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func roundToInt(f float64) int {
	return int(f + 0.5)
}

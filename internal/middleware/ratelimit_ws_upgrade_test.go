package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"9router/proxy/internal/models"
)

// wsRateLimiter builds a limiter sized for the slot tests below.
func wsRateLimiter() *RateLimiter {
	return NewRateLimiter(time.Minute)
}

// withAuthKey injects an authenticated key the same way RequireApiKey leaves
// the context, so RequireRateLimit sees it without a database.
func withAuthKey(r *http.Request) *http.Request {
	key := &models.APIKey{ID: "key-ws-test", Key: "sk-ws-test"}
	return r.WithContext(context.WithValue(r.Context(), ApiKeyContextKey, key))
}

// TestRateLimit_WSUpgradeDoesNotHoldConcurrencySlot is the highest-risk seam of
// the realtime wave: the per-key concurrency slot is released by a defer, so a
// hijacked socket would hold it for the whole voice session and one idle call
// could lock its key out. The WS upgrade must be admitted without taking a
// slot, while the handshake still counts as a request (RPM).
func TestRateLimit_WSUpgradeDoesNotHoldConcurrencySlot(t *testing.T) {
	rl := wsRateLimiter()

	// 1. Hold the only slot with a blocked normal request.
	release := make(chan struct{})
	started := make(chan struct{})
	holder := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
	})
	holderChain := RequireRateLimit(rl, func() Limits { return Limits{Concurrency: 1, RPM: 60} }, nil)(holder)

	holderRec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		holderChain.ServeHTTP(holderRec, withAuthKey(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)))
	}()
	<-started

	// 2. The normal route is refused while the slot is held.
	refused := httptest.NewRecorder()
	holderChain.ServeHTTP(refused, withAuthKey(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)))
	if refused.Code != http.StatusTooManyRequests {
		t.Fatalf("second normal request status = %d, want 429 (slot held)", refused.Code)
	}

	// 3. A WS upgrade on /v1/realtime is admitted anyway: the handshake takes
	// an RPM stamp but no concurrency slot.
	upgradeChain := RequireRateLimit(rl, func() Limits { return Limits{Concurrency: 1, RPM: 60} }, nil)(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	wsRec := httptest.NewRecorder()
	wsReq := withAuthKey(httptest.NewRequest(http.MethodGet, "/v1/realtime?apiKey=sk-ws-test", nil))
	wsReq.Header.Set("Upgrade", "websocket")
	wsReq.Header.Set("Connection", "Upgrade")
	upgradeChain.ServeHTTP(wsRec, wsReq)
	if wsRec.Code != http.StatusOK {
		t.Fatalf("WS upgrade with the slot held status = %d, want 200: %s", wsRec.Code, wsRec.Body.String())
	}

	// 4. The slot is still held by step 1 — the upgrade did not free or take it.
	stillRefused := httptest.NewRecorder()
	holderChain.ServeHTTP(stillRefused, withAuthKey(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)))
	if stillRefused.Code != http.StatusTooManyRequests {
		t.Fatalf("normal request after the WS handshake status = %d, want 429 (slot must stay held by the first request)", stillRefused.Code)
	}

	close(release)
	<-done
}

// TestRateLimit_RequestLoggerPreservesHijacker pins the middleware property the
// whole WS wave depends on: middleware.RequestLogger's statusWriter must stay
// an http.Hijacker, or gorilla answers every upgrade with a 500 (the same
// failure shape the Flush forwarding fixed for SSE). Sits here because it is
// part of the WS seam set.
func TestRateLimit_RequestLoggerPreservesHijacker(t *testing.T) {
	upgrader := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, req, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			return
		}
		_ = conn.Close()
	})
	srv := httptest.NewServer(RequestLogger(upgrader))
	defer srv.Close()

	conn, _, err := (&websocket.Dialer{}).Dial("ws"+srv.URL[4:]+"/realtime", nil)
	if err != nil {
		t.Fatalf("dial through RequestLogger: %v", err)
	}
	_ = conn.Close()
}

// TestRateLimit_WSUpgradeKeepsHandshakeRateLimit pins the other half of the
// contract: skipping the concurrency slot must not skip request-rate limiting —
// the handshake still consumes an RPM stamp.
func TestRateLimit_WSUpgradeKeepsHandshakeRateLimit(t *testing.T) {
	rl := wsRateLimiter()
	chain := RequireRateLimit(rl, func() Limits { return Limits{RPM: 1} }, nil)(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	send := func() *httptest.ResponseRecorder {
		req := withAuthKey(httptest.NewRequest(http.MethodGet, "/v1/realtime", nil))
		req.Header.Set("Upgrade", "websocket")
		rec := httptest.NewRecorder()
		chain.ServeHTTP(rec, req)
		return rec
	}

	if rec := send(); rec.Code != http.StatusOK {
		t.Fatalf("first handshake status = %d, want 200", rec.Code)
	}
	if rec := send(); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second handshake status = %d, want 429 (handshake rate limiting kept)", rec.Code)
	}
}

// TestRateLimit_NonUpgradeGETStillHoldsSlot pins that only the actual WebSocket
// handshake skips the slot: the GET info document on the same path is a short
// request and behaves like every other route.
func TestRateLimit_NonUpgradeGETStillHoldsSlot(t *testing.T) {
	rl := wsRateLimiter()
	release := make(chan struct{})
	started := make(chan struct{})
	holder := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
	})
	chain := RequireRateLimit(rl, func() Limits { return Limits{Concurrency: 1} }, nil)(holder)

	rec := httptest.NewRecorder()
	go func() {
		chain.ServeHTTP(rec, withAuthKey(httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)))
	}()
	<-started

	plain := httptest.NewRecorder()
	chain.ServeHTTP(plain, withAuthKey(httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)))
	if plain.Code != http.StatusTooManyRequests {
		t.Fatalf("plain GET without Upgrade header status = %d, want 429 (it must hold the slot)", plain.Code)
	}

	close(release)
}

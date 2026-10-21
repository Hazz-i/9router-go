// Upstream socket plumbing shared by the four backends: dialing, guarded
// sends, and the error/close event frames.
package caption

import (
	"fmt"
	"net/http"

	"github.com/gorilla/websocket"
)

// setUpstream records the dialled socket so the forward paths and cleanup can
// reach it.
func (s *session) setUpstream(conn *websocket.Conn) {
	s.upMu.Lock()
	s.up = conn
	s.upMu.Unlock()
}

// upstreamOpen reports whether the upstream socket is usable for a send.
func (s *session) upstreamOpen() bool {
	s.upMu.Lock()
	defer s.upMu.Unlock()
	return s.up != nil && !s.upClosed
}

// sendUpstream writes one JSON frame to the upstream socket when it is open.
func (s *session) sendUpstream(v any) {
	s.upMu.Lock()
	up, open := s.up, !s.upClosed
	s.upMu.Unlock()
	if open {
		_ = up.WriteJSON(v)
	}
}

// forwardBinary forwards raw audio bytes to the upstream when it is open.
func (s *session) forwardBinary(mt int, data []byte) {
	s.upMu.Lock()
	up, open := s.up, !s.upClosed
	s.upMu.Unlock()
	if open {
		_ = up.WriteMessage(mt, data)
	}
}

// markUpstreamClosed marks the socket unusable after a dial failure or close.
func (s *session) markUpstreamClosed() {
	s.upMu.Lock()
	s.upClosed = true
	s.upMu.Unlock()
}

// currentUpstream snapshots the upstream socket pointer.
func (s *session) currentUpstream() *websocket.Conn {
	s.upMu.Lock()
	defer s.upMu.Unlock()
	return s.up
}

// dialUpstream opens the upstream WebSocket through the connection's proxy
// dialer when one was resolved (s.dial), else the handler's default. A
// failure replicates the upstream ws event pair — an `error` frame and an
// `upstream_closed` frame — and leaves the client socket open, so it never
// surfaces as a setup throw.
func (s *session) dialUpstream(prefix, url string, header http.Header) {
	dialer := s.dial
	if dialer == nil {
		dialer = &s.h.dial
	}
	up, _, err := dialer.DialContext(s.ctx, url, header) //nolint:bodyclose // gorilla: on success the handshake body IS the socket, on failure it need not be closed.
	if err != nil {
		s.markUpstreamClosed()
		s.upstreamError(prefix, err)
		return
	}
	s.setUpstream(up)
}

// upstreamError sends the prefixed error frame upstream emits on its `error`
// event, then the `upstream_closed` frame its close event produces.
func (s *session) upstreamError(prefix string, err error) {
	_ = s.send(map[string]any{"type": "error", "error": fmt.Sprintf("%s: %v", prefix, err)})
	s.announceUpstreamClosed(1006, "")
}

// announceUpstreamClosed ports the close-event handler: an upstream_closed
// frame that leaves the client socket open.
func (s *session) announceUpstreamClosed(code int, reason string) {
	_ = s.send(map[string]any{"type": "upstream_closed", "code": code, "reason": reason})
}

// orStr substitutes def for an empty string.
func orStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

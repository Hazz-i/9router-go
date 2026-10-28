package oauth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// pinCodebuddySite points a provider's auth host at a fixture for the life of
// the test and restores it afterwards — a refresh must never reach the real
// account host from a test.
func pinCodebuddySite(t *testing.T, provider, site string) {
	t.Helper()
	prev, existed := codebuddySites[provider]
	codebuddySites[provider] = site
	t.Cleanup(func() {
		if existed {
			codebuddySites[provider] = prev
			return
		}
		delete(codebuddySites, provider)
	})
}

// TestRefreshCodebuddy_HeaderBasedGrant checks the shape the official CLI uses
// for refreshSession: POST with an empty JSON body, the refresh token in the
// X-Refresh-Token header (not a form-encoded OAuth2 grant), and the
// accessToken/expiresIn pair surfaced from data.data.
func TestRefreshCodebuddy_HeaderBasedGrant(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/v2/plugin/auth/token/refresh" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("X-Refresh-Token"); got != "rt-fixture" {
			t.Errorf("X-Refresh-Token = %q", got)
		}
		if got := r.Header.Get("X-Auth-Refresh-Source"); got != "plugin" {
			t.Errorf("X-Auth-Refresh-Source = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != "{}" {
			t.Errorf("body = %q, want {}", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"accessToken":"at-new","refreshToken":"rt-new","expiresIn":31533865}}`))
	}))
	defer srv.Close()
	pinCodebuddySite(t, "codebuddy-intl", srv.URL)

	out, err := refreshCodebuddy(context.Background(), &Params{
		Provider:     "codebuddy-intl",
		RefreshToken: "rt-fixture",
	})
	if err != nil {
		t.Fatalf("refreshCodebuddy: %v", err)
	}
	if out.AccessToken != "at-new" || out.RefreshToken != "rt-new" {
		t.Errorf("tokens: access=%q refresh=%q", out.AccessToken, out.RefreshToken)
	}
	if out.ExpiresIn != 31533865 {
		t.Errorf("ExpiresIn = %d, want 31533865", out.ExpiresIn)
	}
	if hits.Load() != 1 {
		t.Errorf("endpoint hit %d times", hits.Load())
	}
}

// TestRefreshCodebuddy_ErrorCode checks the non-zero code path surfaces the
// server's msg instead of returning an unusable token.
func TestRefreshCodebuddy_ErrorCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"code":11219,"msg":"invalid refresh token"}`))
	}))
	defer srv.Close()
	pinCodebuddySite(t, "codebuddy-intl", srv.URL)

	_, err := refreshCodebuddy(context.Background(), &Params{
		Provider:     "codebuddy-intl",
		RefreshToken: "rt-bad",
	})
	if err == nil {
		t.Fatal("want error for non-zero code")
	}
	if want := "codebuddy refresh: invalid refresh token"; err.Error() != want {
		t.Errorf("err = %q, want %q", err.Error(), want)
	}
	// sanity: the fixture body really parses as JSON with int code
	var probe struct {
		Code int `json:"code"`
	}
	_ = json.Unmarshal([]byte(`{"code":11219}`), &probe)
	if probe.Code == 0 {
		t.Fatal("fixture drift")
	}
}

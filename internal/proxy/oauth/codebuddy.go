package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"9router/proxy/internal/providers"
)

func init() {
	Register("codebuddy-cn", refreshCodebuddy)
	Register("codebuddy-intl", refreshCodebuddy)
}

// codebuddySites lets tests point a provider's auth host at a fixture server.
// Same reasoning as miniMaxCodeSites: a refresh must never reach the live
// account host from a test.
var codebuddySites = map[string]string{}

// codebuddyBase returns the plugin-auth host for the provider variant.
func codebuddyBase(provider string) string {
	if site := codebuddySites[provider]; site != "" {
		return site
	}
	if provider == "codebuddy-cn" {
		return "https://copilot.tencent.com"
	}
	return "https://www.codebuddy.ai"
}

// refreshCodebuddy mirrors the official CLI's refreshSession: POST
// /v2/plugin/auth/token/refresh with the refresh token in the X-Refresh-Token
// header and an empty JSON body — not the form-encoded OAuth2 grant the
// standard refresher sends, which the endpoint ignores. The response `data`
// object carries accessToken/refreshToken/expiresIn (seconds, ~1 year).
// Verified live: 200 {code:0, data:{accessToken, expiresIn, refreshToken,...}}.
func refreshCodebuddy(ctx context.Context, p *Params) (*TokenResult, error) {
	if strings.TrimSpace(p.RefreshToken) == "" {
		return nil, fmt.Errorf("codebuddy refresh: missing refresh token")
	}
	base := codebuddyBase(p.Provider)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base+"/v2/plugin/auth/token/refresh", strings.NewReader("{}"))
	if err != nil {
		return nil, fmt.Errorf("codebuddy refresh: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "CLI/"+providers.CodeBuddyClientVersion+" CodeBuddy/"+providers.CodeBuddyClientVersion)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("X-Domain", strings.TrimPrefix(base, "https://"))
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("X-Refresh-Token", p.RefreshToken)
	req.Header.Set("X-Auth-Refresh-Source", "plugin")

	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codebuddy refresh: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("codebuddy refresh: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		preview := string(body)
		if len(preview) > 200 {
			preview = preview[:200]
		}
		return nil, fmt.Errorf("codebuddy refresh: HTTP %d: %s", resp.StatusCode, preview)
	}
	var data struct {
		Code float64 `json:"code"`
		Msg  string  `json:"msg"`
		Data struct {
			AccessToken  string  `json:"accessToken"`
			RefreshToken string  `json:"refreshToken"`
			ExpiresIn    float64 `json:"expiresIn"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &data) != nil {
		return nil, fmt.Errorf("codebuddy refresh: invalid JSON")
	}
	if data.Code != 0 || data.Data.AccessToken == "" {
		return nil, fmt.Errorf("codebuddy refresh: %s", data.Msg)
	}
	return &TokenResult{
		AccessToken:  data.Data.AccessToken,
		RefreshToken: data.Data.RefreshToken,
		ExpiresIn:    int(data.Data.ExpiresIn),
	}, nil
}

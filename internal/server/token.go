// Token-vending endpoint: the central server holds app credentials and
// issues tenant_access_token to sub-servers. Sub-servers then call the
// Feishu OpenAPI directly; download traffic never transits the center.
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// TokenVender serves GET /token -> {"tenant_access_token": "...",
// "expire": <seconds>}. It mints tokens from app credentials and caches
// them until shortly before expiry.
type TokenVender struct {
	appID     string
	appSecret string
	tokenURL  string // Feishu tenant_access_token endpoint
	vendKey   string // optional shared key; "" = open (bind trusted net!)

	mu   sync.Mutex
	tok  string
	exp  time.Time
	http *http.Client
}

// NewTokenVender builds a vender for the given app credentials.
// base is the OpenAPI host (e.g. https://open.feishu.cn).
// vendKey, when non-empty, requires requests to carry the same value in
// the X-Vend-Key header.
func NewTokenVender(appID, appSecret, base, vendKey string) *TokenVender {
	return &TokenVender{
		appID:     appID,
		appSecret: appSecret,
		tokenURL:  base + "/open-apis/auth/v3/tenant_access_token/internal",
		vendKey:   vendKey,
		http:      &http.Client{Timeout: 15 * time.Second},
	}
}

func (v *TokenVender) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/token" {
		http.NotFound(w, r)
		return
	}
	if v.vendKey != "" && r.Header.Get("X-Vend-Key") != v.vendKey {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	tok, expireIn, err := v.Mint()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"tenant_access_token": tok,
		"expire":              expireIn,
	})
}

// Mint returns a cached-or-fresh token and its remaining lifetime in seconds.
func (v *TokenVender) Mint() (string, int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.tok != "" && time.Now().Before(v.exp) {
		return v.tok, int(time.Until(v.exp).Seconds()), nil
	}
	body, _ := json.Marshal(map[string]string{
		"app_id":     v.appID,
		"app_secret": v.appSecret,
	})
	resp, err := v.http.Post(v.tokenURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", 0, err
	}
	var out struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
		Expire            int    `json:"expire"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", 0, fmt.Errorf("token endpoint parse: %w", err)
	}
	if out.Code != 0 || out.TenantAccessToken == "" {
		return "", 0, fmt.Errorf("token endpoint error: code=%d msg=%s", out.Code, out.Msg)
	}
	v.tok = out.TenantAccessToken
	// Re-mint 5 minutes early.
	v.exp = time.Now().Add(time.Duration(max(out.Expire-300, 60)) * time.Second)
	return v.tok, int(time.Until(v.exp).Seconds()), nil
}

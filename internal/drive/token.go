package drive

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	nurl "net/url"
	"sync"
	"time"
)

// tokenSource supplies a tenant_access_token on demand. Central holds
// app credentials; sub-servers only ever see the delegated token.
type tokenSource interface {
	token(ctx context.Context) (string, error)
}

// staticToken is a manually supplied tenant_access_token.
type staticToken struct{ s string }

func (t staticToken) token(context.Context) (string, error) { return t.s, nil }

// remoteTokenSource fetches tokens from a central vend endpoint
// (GET -> {"tenant_access_token": "...", "expire": <seconds>}) and
// caches them until shortly before expiry.
type remoteTokenSource struct {
	url    string
	mu     sync.Mutex
	tok    string
	exp    time.Time
	client *http.Client
}

// vendResponse mirrors Feishu's tenant_access_token response shape.
type vendResponse struct {
	TenantAccessToken string `json:"tenant_access_token"`
	Expire            int    `json:"expire"` // seconds
	Msg               string `json:"msg"`
}

func newRemoteTokenSource(url string) *remoteTokenSource {
	// Accept a bare host[:port] and default to the /token endpoint.
	if u, err := nurl.Parse(url); err == nil && (u.Path == "" || u.Path == "/") {
		u.Path = "/token"
		url = u.String()
	}
	return &remoteTokenSource{
		url:    url,
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

func (s *remoteTokenSource) token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tok != "" && time.Now().Before(s.exp) {
		return s.tok, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return "", err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var v vendResponse
	if err := json.Unmarshal(body, &v); err != nil {
		return "", fmt.Errorf("token vend response parse: %w (status=%d)", err, resp.StatusCode)
	}
	if v.TenantAccessToken == "" {
		return "", fmt.Errorf("token vend returned no token (status=%d msg=%s)", resp.StatusCode, v.Msg)
	}
	s.tok = v.TenantAccessToken
	// Refresh 5 minutes early.
	s.exp = time.Now().Add(time.Duration(max(v.Expire-300, 60)) * time.Second)
	return s.tok, nil
}

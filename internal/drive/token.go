package drive

import "context"

// tokenSource supplies a tenant_access_token on demand.
type tokenSource interface {
	token(ctx context.Context) (string, error)
}

// staticToken is a manually supplied tenant_access_token
// (LARK_TENANT_TOKEN) — for running without app credentials.
type staticToken struct{ s string }

func (t staticToken) token(context.Context) (string, error) { return t.s, nil }

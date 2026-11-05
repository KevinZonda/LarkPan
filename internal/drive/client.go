// Package drive wraps the Lark OpenAPI SDK for app-identity (tenant_access_token)
// access to a Feishu Drive folder tree.
package drive

import (
	"context"
	"fmt"
	"os"
	"strconv"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
)

// Client wraps the lark SDK client scoped to Drive operations.
type Client struct {
	raw                 *lark.Client
	uploadConcurrency   int
	downloadConcurrency int
	ts                  tokenSource // non-nil in delegated-token mode
}

// Options controls client construction.
type Options struct {
	// Domain is "feishu" (default) or "lark" (larksuite.com international).
	Domain string
	// UploadConcurrency is the number of chunk parts uploaded in parallel.
	// Values <= 0 fall back to 4.
	UploadConcurrency int
	// DownloadConcurrency is the number of files downloaded in parallel.
	// Values <= 0 fall back to 4.
	DownloadConcurrency int
}

// New builds a Client from explicit app credentials.
func New(appID, appSecret string, opts Options) (*Client, error) {
	if appID == "" || appSecret == "" {
		return nil, fmt.Errorf("app id and app secret are required")
	}
	base := lark.FeishuBaseUrl
	if opts.Domain == "lark" {
		base = lark.LarkBaseUrl
	}
	conc := opts.UploadConcurrency
	if conc <= 0 {
		conc = 4
	}
	dconc := opts.DownloadConcurrency
	if dconc <= 0 {
		dconc = 4
	}
	return &Client{
		raw:                 lark.NewClient(appID, appSecret, lark.WithOpenBaseUrl(base)),
		uploadConcurrency:   conc,
		downloadConcurrency: dconc,
	}, nil
}

// NewFromEnv builds a Client. Credential precedence:
//
//	LARK_TENANT_TOKEN   - delegated token mode (static token, no app creds)
//	LARK_APP_ID/SECRET  - app mode (mints its own tokens)
//
// LARK_DOMAIN selects feishu (default) vs lark; concurrency via
// LARK_UPLOAD_CONCURRENCY / LARK_DOWNLOAD_CONCURRENCY.
func NewFromEnv() (*Client, error) {
	uconc, _ := strconv.Atoi(os.Getenv("LARK_UPLOAD_CONCURRENCY"))
	dconc, _ := strconv.Atoi(os.Getenv("LARK_DOWNLOAD_CONCURRENCY"))
	opts := Options{
		Domain:              os.Getenv("LARK_DOMAIN"),
		UploadConcurrency:   uconc,
		DownloadConcurrency: dconc,
	}
	if t := os.Getenv("LARK_TENANT_TOKEN"); t != "" {
		return NewToken(staticToken{s: t}, opts)
	}
	return New(os.Getenv("LARK_APP_ID"), os.Getenv("LARK_APP_SECRET"), opts)
}

// NewToken builds a Client that injects a delegated tenant_access_token on
// every request instead of minting one from app credentials.
func NewToken(ts tokenSource, opts Options) (*Client, error) {
	c, err := New("token-mode", "token-mode", opts)
	if err != nil {
		return nil, err
	}
	c.ts = ts
	return c, nil
}

// reqOpts returns request options carrying the delegated token, if any.
func (c *Client) reqOpts(ctx context.Context) ([]larkcore.RequestOptionFunc, error) {
	if c.ts == nil {
		return nil, nil
	}
	t, err := c.ts.token(ctx)
	if err != nil {
		return nil, err
	}
	return []larkcore.RequestOptionFunc{larkcore.WithTenantAccessToken(t)}, nil
}

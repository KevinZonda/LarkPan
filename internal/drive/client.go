// Package drive wraps the Lark OpenAPI SDK for app-identity (tenant_access_token)
// access to a Feishu Drive folder tree.
package drive

import (
	"fmt"
	"os"

	lark "github.com/larksuite/oapi-sdk-go/v3"
)

// Client wraps the lark SDK client scoped to Drive operations.
type Client struct {
	raw *lark.Client
}

// Options controls client construction.
type Options struct {
	// Domain is "feishu" (default) or "lark" (larksuite.com international).
	Domain string
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
	return &Client{raw: lark.NewClient(appID, appSecret, lark.WithOpenBaseUrl(base))}, nil
}

// NewFromEnv builds a Client from LARK_APP_ID / LARK_APP_SECRET / LARK_DOMAIN.
func NewFromEnv() (*Client, error) {
	return New(os.Getenv("LARK_APP_ID"), os.Getenv("LARK_APP_SECRET"),
		Options{Domain: os.Getenv("LARK_DOMAIN")})
}

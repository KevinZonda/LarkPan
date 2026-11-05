// Package sign implements HMAC-signed URLs for the edge server, S3
// presigned-URL style: a URL carries expires + sig, verifiable with only
// the edge secret. Rotating the secret kills every issued URL.
package sign

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strconv"
	"time"
)

// Token produces the signature for a URL path valid until expiresAt.
func Token(secret, path string, expiresAt int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(path))
	mac.Write([]byte{0})
	mac.Write([]byte(strconv.FormatInt(expiresAt, 10)))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:16])
}

// Valid reports whether r carries a live signature for its path.
func Valid(secret string, r *http.Request, now time.Time) bool {
	exp, err := strconv.ParseInt(r.URL.Query().Get("expires"), 10, 64)
	if err != nil || now.Unix() > exp {
		return false
	}
	sig := r.URL.Query().Get("sig")
	if sig == "" {
		return false
	}
	return hmac.Equal([]byte(Token(secret, r.URL.Path, exp)), []byte(sig))
}

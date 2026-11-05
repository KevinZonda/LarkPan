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
	"strings"
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

// --- long-lived client tokens ---------------------------------------------
//
// Format: lp_<label>_<expiry>_<sig>. Issued by the edge secret holder;
// verified statelessly with the same secret. Revoke by rotating the secret.

// ClientToken mints a bearer token for a long-lived client credential.
func ClientToken(secret, label string, expiresAt int64) string {
	body := "client\x00" + label + "\x00" + strconv.FormatInt(expiresAt, 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:16])
	return "lp_" + label + "_" + strconv.FormatInt(expiresAt, 10) + "_" + sig
}

// ValidBearer reports whether the Authorization bearer token is a live
// client token under secret.
func ValidBearer(secret, token string, now time.Time) bool {
	rest, ok := strings.CutPrefix(token, "lp_")
	if !ok {
		return false
	}
	parts := strings.Split(rest, "_")
	if len(parts) < 3 {
		return false
	}
	sig := parts[len(parts)-1]
	exp, err := strconv.ParseInt(parts[len(parts)-2], 10, 64)
	if err != nil || now.Unix() > exp {
		return false
	}
	label := strings.Join(parts[:len(parts)-2], "_")
	return hmac.Equal([]byte(ClientToken(secret, label, exp)), []byte(token)) ||
		hmac.Equal([]byte(clientSig(secret, label, exp)), []byte(sig))
}

func clientSig(secret, label string, expiresAt int64) string {
	body := "client\x00" + label + "\x00" + strconv.FormatInt(expiresAt, 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:16])
}

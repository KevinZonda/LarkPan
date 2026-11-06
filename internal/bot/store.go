// Package bot implements Feishu-bot token issuance: users DM the app's
// bot over the SDK long connection (no public callback URL needed) and
// the sender's verified open_id becomes the lp_ token label.
package bot

import (
	"crypto/rand"
	"sync"
	"time"
)

// LoginState is one pending CLI login pairing: created by POST /auth/pair,
// bound to a Feishu identity when a user DMs `login <code>` to the bot,
// and redeemed once by GET /auth/poll.
type LoginState struct {
	Code     string
	Expires  time.Time
	Issued   bool
	Token    string
	Label    string
	TokenExp int64
}

// LoginStore is an in-memory registry of pairing codes.
type LoginStore struct {
	mu  sync.Mutex
	m   map[string]*LoginState
	ttl time.Duration
	max int
}

// NewLoginStore returns a store with 5-minute pairing codes and room for
// 256 concurrent pending logins.
func NewLoginStore() *LoginStore {
	return &LoginStore{m: map[string]*LoginState{}, ttl: 5 * time.Minute, max: 256}
}

// Create registers a fresh pairing code; nil when the store is full.
func (s *LoginStore) Create() *LoginState {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for c, st := range s.m {
		if now.After(st.Expires) {
			delete(s.m, c)
		}
	}
	if len(s.m) >= s.max {
		return nil
	}
	for {
		code := pairCode()
		if _, dup := s.m[code]; !dup {
			st := &LoginState{Code: code, Expires: now.Add(s.ttl)}
			s.m[code] = st
			return st
		}
	}
}

// Get returns the state for code, dropping it if expired.
func (s *LoginStore) Get(code string) (*LoginState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.get(code)
}

func (s *LoginStore) get(code string) (*LoginState, bool) {
	st, ok := s.m[code]
	if !ok {
		return nil, false
	}
	if time.Now().After(st.Expires) {
		delete(s.m, code)
		return nil, false
	}
	return st, true
}

// Issue binds an issued token to the pairing code (bot side).
func (s *LoginStore) Issue(code, token, label string, tokenExp int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.get(code)
	if !ok {
		return false
	}
	st.Issued, st.Token, st.Label, st.TokenExp = true, token, label, tokenExp
	return true
}

// Redeem returns and deletes the state once a token has been issued —
// delivery is single-use (CLI side).
func (s *LoginStore) Redeem(code string) (*LoginState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.get(code)
	if !ok || !st.Issued {
		return nil, false
	}
	delete(s.m, code)
	return st, true
}

// pairCode makes a 6-char code from an unambiguous alphabet (~31 bits).
func pairCode() string {
	const alpha = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = alpha[int(b[i])%len(alpha)]
	}
	return string(b[:])
}

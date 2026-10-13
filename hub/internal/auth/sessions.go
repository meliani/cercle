package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/meliani/cercle/hub/internal/db"
)

// ErrNoSession means the token is unknown or expired.
var ErrNoSession = errors.New("no session")

const (
	SessionTTL     = 30 * 24 * time.Hour
	refreshEvery   = 24 * time.Hour
	tokenRandBytes = 32
)

// HashToken returns the hex sha256 of a token, which is all we store.
func HashToken(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

// Sessions stores hub sessions.
type Sessions struct {
	DB  *db.DB
	Now func() time.Time
}

// Create issues a new opaque token for username.
func (s *Sessions) Create(ctx context.Context, username string) (string, error) {
	b := make([]byte, tokenRandBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(b)
	now := s.Now()
	_, err := s.DB.W.ExecContext(ctx, `INSERT INTO sessions (token_hash, username, created_at, expires_at, refreshed_at)
		VALUES (?, ?, ?, ?, ?)`, HashToken(tok), username, now.UnixMilli(), now.Add(SessionTTL).UnixMilli(), now.UnixMilli())
	if err != nil {
		return "", err
	}
	return tok, nil
}

// Lookup resolves a token to its username, sliding the expiry at most once a day.
func (s *Sessions) Lookup(ctx context.Context, tok string) (string, error) {
	if tok == "" {
		return "", ErrNoSession
	}
	h := HashToken(tok)
	var user string
	var exp, refreshed int64
	err := s.DB.R.QueryRowContext(ctx, `SELECT username, expires_at, refreshed_at FROM sessions WHERE token_hash = ?`, h).
		Scan(&user, &exp, &refreshed)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoSession
	}
	if err != nil {
		return "", err
	}
	now := s.Now()
	if exp <= now.UnixMilli() {
		_, _ = s.DB.W.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, h)
		return "", ErrNoSession
	}
	if now.UnixMilli()-refreshed >= refreshEvery.Milliseconds() {
		_, _ = s.DB.W.ExecContext(ctx, `UPDATE sessions SET expires_at = ?, refreshed_at = ? WHERE token_hash = ?`,
			now.Add(SessionTTL).UnixMilli(), now.UnixMilli(), h)
	}
	return user, nil
}

// Delete revokes a token.
func (s *Sessions) Delete(ctx context.Context, tok string) error {
	_, err := s.DB.W.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, HashToken(tok))
	return err
}

// Prune removes expired sessions.
func (s *Sessions) Prune(ctx context.Context) error {
	_, err := s.DB.W.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, s.Now().UnixMilli())
	return err
}

// FailLimiter counts failed logins per key (client IP) in a sliding window.
type FailLimiter struct {
	Max    int
	Window time.Duration
	Now    func() time.Time

	mu    sync.Mutex
	fails map[string][]time.Time
}

func NewFailLimiter(max int, window time.Duration, now func() time.Time) *FailLimiter {
	return &FailLimiter{Max: max, Window: window, Now: now, fails: map[string][]time.Time{}}
}

func (l *FailLimiter) trim(key string, now time.Time) []time.Time {
	ts := l.fails[key]
	i := 0
	for i < len(ts) && now.Sub(ts[i]) >= l.Window {
		i++
	}
	ts = ts[i:]
	if len(ts) == 0 {
		delete(l.fails, key)
	} else {
		l.fails[key] = ts
	}
	return ts
}

// Blocked reports whether key has reached Max failures within Window.
func (l *FailLimiter) Blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.trim(key, l.Now())) >= l.Max
}

// Fail records a failure for key.
func (l *FailLimiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.Now()
	l.fails[key] = append(l.trim(key, now), now)
	if len(l.fails) > 10000 { // bound memory under a spray of IPs
		for k := range l.fails {
			l.trim(k, now)
		}
	}
}

// ClientIP returns the request's client address. X-Forwarded-For is only
// honoured when the direct peer is a trusted proxy; then the right-most
// address that is not itself a trusted proxy wins.
func ClientIP(r *http.Request, trusted []netip.Prefix) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil || !inPrefixes(peer, trusted) {
		return host
	}
	xff := r.Header.Values("X-Forwarded-For")
	var parts []string
	for _, h := range xff {
		parts = append(parts, strings.Split(h, ",")...)
	}
	for i := len(parts) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(parts[i]))
		if err != nil {
			break
		}
		if !inPrefixes(a, trusted) {
			return a.String()
		}
	}
	return host
}

// FromTrustedProxy reports whether the direct peer is in trusted.
func FromTrustedProxy(r *http.Request, trusted []netip.Prefix) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	a, err := netip.ParseAddr(host)
	return err == nil && inPrefixes(a, trusted)
}

func inPrefixes(a netip.Addr, ps []netip.Prefix) bool {
	a = a.Unmap()
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// BearerToken extracts "Authorization: Bearer <tok>".
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

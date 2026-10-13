package api

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/meliani/cercle/hub/internal/config"
)

func postJSON(t *testing.T, url string, body any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestSessionFlow(t *testing.T) {
	e := newEnv(t)
	url := e.ts.URL + "/hub/api/v1/auth/session"

	code, out := postJSON(t, url, map[string]string{"username": "alice", "password": "pw-alice"})
	if code != 200 {
		t.Fatalf("password login: %d %v", code, out)
	}
	tok, _ := out["token"].(string)
	if len(tok) != 43 { // 32 bytes base64url without padding
		t.Fatalf("token %q", tok)
	}
	user := out["user"].(map[string]any)
	if user["username"] != "alice" || user["is_admin"] != true {
		t.Fatalf("user: %v", user)
	}
	// Only the hash is stored.
	var n int
	if err := e.srv.DB.R.QueryRow(`SELECT COUNT(*) FROM sessions WHERE token_hash = ?`, tok).Scan(&n); err != nil || n != 0 {
		t.Fatal("raw token must not be stored")
	}

	salt := "abc123"
	sum := md5.Sum([]byte("pw-bob" + salt))
	if code, out := postJSON(t, url, map[string]string{"username": "bob", "token": hex.EncodeToString(sum[:]), "salt": salt}); code != 200 {
		t.Fatalf("token+salt login: %d %v", code, out)
	}
	if code, out := postJSON(t, url, map[string]string{"username": "carol", "api_key": "key-carol"}); code != 200 {
		t.Fatalf("api key login: %d %v", code, out)
	}
	if code, _ := postJSON(t, url, map[string]string{"username": "alice", "api_key": "key-carol"}); code != 401 {
		t.Fatalf("api key of another user: %d", code)
	}
	if code, _ := postJSON(t, url, map[string]string{"username": "alice", "password": "nope"}); code != 401 {
		t.Fatalf("wrong password: %d", code)
	}
	if code, _ := postJSON(t, url, map[string]string{"username": "alice"}); code != 400 {
		t.Fatalf("no credentials: %d", code)
	}

	// The token works, sliding expiry keeps it alive, logout kills it.
	e.tokens["alice"] = tok
	e.must(200, "GET", "/api/v1/me", "alice", nil, nil)
	e.clock.Advance(29 * 24 * time.Hour)
	e.must(200, "GET", "/api/v1/me", "alice", nil, nil) // refreshes
	e.clock.Advance(29 * 24 * time.Hour)
	e.must(200, "GET", "/api/v1/me", "alice", nil, nil)
	e.clock.Advance(31 * 24 * time.Hour)
	if code := e.req("GET", "/api/v1/me", "alice", nil, nil); code != 401 {
		t.Fatalf("expired session: %d", code)
	}
	delete(e.tokens, "alice")
	e.must(204, "DELETE", "/api/v1/auth/session", "alice", nil, nil)
	if code := e.req("GET", "/api/v1/me", "alice", nil, nil); code != 401 {
		t.Fatalf("after logout: %d", code)
	}
	r, _ := http.NewRequest("GET", e.ts.URL+"/hub/api/v1/me", nil)
	resp, _ := http.DefaultClient.Do(r)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
}

func TestNavidromeDown(t *testing.T) {
	e := newEnv(t)
	url := e.ts.URL + "/hub/api/v1/auth/session"
	e.nav.setDown(true)
	if code, out := postJSON(t, url, map[string]string{"username": "alice", "password": "pw-alice"}); code != 502 {
		t.Fatalf("navidrome 500 → 502, got %d %v", code, out)
	}
	e.nav.setDown(false)
	e.navTS.Close() // unreachable
	if code, _ := postJSON(t, url, map[string]string{"username": "alice", "password": "pw-alice"}); code != 502 {
		t.Fatalf("navidrome unreachable → 502, got %d", code)
	}
}

func TestLoginRateLimit(t *testing.T) {
	e := newEnv(t)
	url := e.ts.URL + "/hub/api/v1/auth/session"
	for i := 0; i < 10; i++ {
		if code, _ := postJSON(t, url, map[string]string{"username": "alice", "password": "bad"}); code != 401 {
			t.Fatalf("attempt %d: %d", i, code)
		}
	}
	calls := e.nav.callCount()
	if code, _ := postJSON(t, url, map[string]string{"username": "alice", "password": "pw-alice"}); code != 429 {
		t.Fatalf("11th attempt: %d, want 429", code)
	}
	if e.nav.callCount() != calls {
		t.Fatal("blocked attempts must not reach Navidrome")
	}
	e.clock.Advance(61 * time.Second)
	if code, _ := postJSON(t, url, map[string]string{"username": "alice", "password": "pw-alice"}); code != 200 {
		t.Fatalf("after the window: %d", code)
	}
}

func TestBasePathHealthAndCORS(t *testing.T) {
	e := newEnv(t)
	get := func(path string, hdr map[string]string, method string) *http.Response {
		r, _ := http.NewRequest(method, e.ts.URL+path, nil)
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp
	}
	if r := get("/hub/health", nil, "GET"); r.StatusCode != 200 {
		t.Fatalf("/hub/health %d", r.StatusCode)
	}
	if r := get("/health", nil, "GET"); r.StatusCode != 200 {
		t.Fatalf("/health %d", r.StatusCode)
	}
	if r := get("/api/v1/me", nil, "GET"); r.StatusCode != 404 {
		t.Fatalf("API outside base path: %d", r.StatusCode)
	}
	pre := get("/hub/api/v1/me", map[string]string{"Origin": "https://player.example.com", "Access-Control-Request-Method": "GET",
		"Access-Control-Request-Headers": "authorization"}, "OPTIONS")
	if pre.StatusCode != 204 || pre.Header.Get("Access-Control-Allow-Origin") != "https://player.example.com" ||
		pre.Header.Get("Access-Control-Allow-Headers") == "" {
		t.Fatalf("preflight: %d %v", pre.StatusCode, pre.Header)
	}
	bad := get("/hub/api/v1/me", map[string]string{"Origin": "https://evil.example.com", "Access-Control-Request-Method": "GET"}, "OPTIONS")
	if bad.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("disallowed origin must not get CORS headers")
	}
	simple := get("/hub/health", map[string]string{"Origin": "https://player.example.com"}, "GET")
	if simple.Header.Get("Access-Control-Allow-Origin") != "https://player.example.com" {
		t.Fatal("simple request gets ACAO")
	}
}

func TestTrustedHeader(t *testing.T) {
	e := newEnv(t, func(c *config.Config) {
		c.TrustedHeader = "X-Remote-User"
		c.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}
	})
	r, _ := http.NewRequest("GET", e.ts.URL+"/hub/api/v1/me", nil)
	r.Header.Set("X-Remote-User", "alice")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	var me map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&me)
	resp.Body.Close()
	if resp.StatusCode != 200 || me["username"] != "alice" {
		t.Fatalf("trusted header: %d %v", resp.StatusCode, me)
	}
}

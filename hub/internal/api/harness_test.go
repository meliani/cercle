package api

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meliani/cercle/hub/internal/auth"
	"github.com/meliani/cercle/hub/internal/config"
	"github.com/meliani/cercle/hub/internal/db"
)

const testSecret = "0123456789abcdef0123456789abcdef-test"

// clock is a controllable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakeNavidrome implements the bits of the Subsonic API the hub calls.
type fakeNavidrome struct {
	mu    sync.Mutex
	users map[string]fakeUser
	down  bool
	calls int
	// Native API state.
	jwts        map[string]string // token → username
	jwtSeq      int
	logins      int
	listCalls   int
	listOnly    []string // when set, /api/user lists only these users (prod bug repro)
	rotateToken bool     // when set, /api/user returns a refreshed token header
}

type fakeUser struct {
	password string
	apiKey   string
	admin    bool
	name     string
}

func newFakeNavidrome() *fakeNavidrome {
	return &fakeNavidrome{jwts: map[string]string{}, users: map[string]fakeUser{
		"alice": {password: "pw-alice", apiKey: "key-alice", admin: true, name: "Alice Liddell"},
		"bob":   {password: "pw-bob", apiKey: "key-bob", name: "Bob B."},
		"carol": {password: "pw-carol", apiKey: "key-carol", name: "Carol C."},
		"dave":  {password: "pw-dave", apiKey: "key-dave"},
		"admin": {password: "pw-admin", apiKey: "key-admin", admin: true},
	}}
}

// expireJWTs invalidates every issued native token (forces a re-login).
func (f *fakeNavidrome) expireJWTs() {
	f.mu.Lock()
	f.jwts = map[string]string{}
	f.mu.Unlock()
}

// serveNative implements POST /auth/login and GET /api/user (Navidrome 0.64 shapes).
func (f *fakeNavidrome) serveNative(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/auth/login":
		var in struct{ Username, Password string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.logins++
		u, ok := f.users[in.Username]
		if !ok || u.password != in.Password {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"Invalid username or password"}`)
			return
		}
		f.jwtSeq++
		tok := fmt.Sprintf("jwt-%d-%s", f.jwtSeq, in.Username)
		f.jwts[tok] = in.Username
		_ = json.NewEncoder(w).Encode(map[string]any{"token": tok, "id": "id-" + in.Username, "name": u.name,
			"username": in.Username, "isAdmin": u.admin, "subsonicSalt": "abc", "subsonicToken": "secret"})
	case r.Method == http.MethodGet && r.URL.Path == "/api/user":
		f.listCalls++
		h := r.Header.Get("X-ND-Authorization")
		who, ok := f.jwts[strings.TrimPrefix(h, "Bearer ")]
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if !f.users[who].admin {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		list := []map[string]any{}
		for name, u := range f.users {
			if f.listOnly != nil && !contains(f.listOnly, name) {
				continue
			}
			list = append(list, map[string]any{"id": "id-" + name, "userName": name, "name": u.name, "isAdmin": u.admin,
				"email": name + "@example.com"})
		}
		if f.rotateToken {
			f.jwtSeq++
			fresh := fmt.Sprintf("jwt-%d-%s", f.jwtSeq, who)
			f.jwts[fresh] = who
			w.Header().Set("X-ND-Authorization", fresh)
		}
		w.Header().Set("X-Total-Count", fmt.Sprint(len(list)))
		_ = json.NewEncoder(w).Encode(list)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeNavidrome) addUser(name string, admin bool) {
	f.mu.Lock()
	f.users[name] = fakeUser{password: "pw-" + name, apiKey: "key-" + name, admin: admin, name: strings.ToUpper(name[:1]) + name[1:]}
	f.mu.Unlock()
}

func (f *fakeNavidrome) removeUser(name string) {
	f.mu.Lock()
	delete(f.users, name)
	f.mu.Unlock()
}

func (f *fakeNavidrome) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.down {
		http.Error(w, "upstream exploded", http.StatusInternalServerError)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/rest/") {
		f.serveNative(w, r)
		return
	}
	q := r.URL.Query()
	fail := func() {
		_, _ = io.WriteString(w, `{"subsonic-response":{"status":"failed","version":"1.16.1","error":{"code":40,"message":"Wrong username or password"}}}`)
	}
	who := ""
	if k := q.Get("apiKey"); k != "" {
		for name, u := range f.users {
			if u.apiKey == k {
				who = name
			}
		}
	} else if u, ok := f.users[q.Get("u")]; ok {
		if p := q.Get("p"); p != "" && p == u.password {
			who = q.Get("u")
		} else if t, s := q.Get("t"), q.Get("s"); t != "" {
			sum := md5.Sum([]byte(u.password + s))
			if hex.EncodeToString(sum[:]) == t {
				who = q.Get("u")
			}
		}
	}
	if who == "" {
		fail()
		return
	}
	resp := map[string]any{"status": "ok", "version": "1.16.1"}
	switch r.URL.Path {
	case "/rest/ping.view":
	case "/rest/tokenInfo.view":
		resp["tokenInfo"] = map[string]string{"username": who}
	case "/rest/getUsers.view":
		// Navidrome 0.64: only the calling user, whatever their role.
		resp["users"] = map[string]any{"user": []map[string]any{{"username": who, "adminRole": f.users[who].admin}}}
	case "/rest/getUser.view":
		target := q.Get("username")
		if target != who && !f.users[who].admin {
			_, _ = io.WriteString(w, `{"subsonic-response":{"status":"failed","error":{"code":50,"message":"not authorized"}}}`)
			return
		}
		resp["user"] = map[string]any{"username": target, "adminRole": f.users[target].admin}
	default:
		http.NotFound(w, r)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"subsonic-response": resp})
}

func (f *fakeNavidrome) setDown(v bool) {
	f.mu.Lock()
	f.down = v
	f.mu.Unlock()
}

func (f *fakeNavidrome) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type env struct {
	t      *testing.T
	srv    *Server
	ts     *httptest.Server
	nav    *fakeNavidrome
	navTS  *httptest.Server
	clock  *clock
	base   string
	tokens map[string]string
}

var t0 = time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

type envOpt func(*config.Config)

func newEnv(t *testing.T, opts ...envOpt) *env {
	t.Helper()
	nav := newFakeNavidrome()
	navTS := httptest.NewServer(nav)
	t.Cleanup(navTS.Close)
	cfg := &config.Config{
		Listen: ":0", DBPath: filepath.Join(t.TempDir(), "cercle.db"), NavidromeURL: navTS.URL,
		IngestSecret: testSecret, BasePath: "/hub", Location: time.UTC,
		CORSOrigins: []string{"https://player.example.com"},
	}
	for _, o := range opts {
		o(cfg)
	}
	d, err := db.Open(context.Background(), cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel) // runs before d.Close (LIFO)
	clk := &clock{t: t0}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(cfg, d, auth.NewHTTPNavidrome(navTS.URL), "test", log, clk.Now)
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	<-srv.SyncDone
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { srv.Hub.Close(); ts.Close() })
	return &env{t: t, srv: srv, ts: ts, nav: nav, navTS: navTS, clock: clk, base: cfg.BasePath, tokens: map[string]string{}}
}

// req performs a request as user ("" = anonymous) and decodes JSON into out.
func (e *env) req(method, path, user string, body any, out any) int {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	r, err := http.NewRequest(method, e.ts.URL+e.base+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if user != "" {
		r.Header.Set("Authorization", "Bearer "+e.token(user))
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			e.t.Fatalf("%s %s: decode %q: %v", method, path, data, err)
		}
	}
	return resp.StatusCode
}

// must is req expecting a given status.
func (e *env) must(want int, method, path, user string, body any, out any) {
	e.t.Helper()
	var raw json.RawMessage
	got := e.req(method, path, user, body, &raw)
	if got != want {
		e.t.Fatalf("%s %s as %q: status %d, want %d (body %s)", method, path, user, got, want, raw)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			e.t.Fatalf("decode: %v", err)
		}
	}
}

func (e *env) token(user string) string {
	e.t.Helper()
	if tok, ok := e.tokens[user]; ok {
		return tok
	}
	var resp struct {
		Token string `json:"token"`
	}
	b, _ := json.Marshal(map[string]string{"username": user, "password": "pw-" + user})
	r, err := http.Post(e.ts.URL+e.base+"/api/v1/auth/session", "application/json", bytes.NewReader(b))
	if err != nil {
		e.t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(r.Body)
		e.t.Fatalf("login %s: %d %s", user, r.StatusCode, data)
	}
	if err := json.NewDecoder(r.Body).Decode(&resp); err != nil {
		e.t.Fatal(err)
	}
	e.tokens[user] = resp.Token
	return resp.Token
}

// ingest posts raw events with the ingest secret.
func (e *env) ingest(evs ...map[string]any) int {
	e.t.Helper()
	var body any = map[string]any{"events": evs}
	if len(evs) == 1 {
		body = evs[0]
	}
	b, _ := json.Marshal(body)
	r, _ := http.NewRequest(http.MethodPost, e.ts.URL+e.base+"/ingest/v1/events", bytes.NewReader(b))
	r.Header.Set("Authorization", "Bearer "+testSecret)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Accepted int `json:"accepted"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusAccepted {
		e.t.Fatalf("ingest: status %d", resp.StatusCode)
	}
	return out.Accepted
}

type tr struct {
	id, album, artist string
	durMs             int64
}

func track(id, album, artist string) tr { return tr{id, album, artist, 200000} }

func (t tr) json() map[string]any {
	return map[string]any{"id": t.id, "title": "Title " + t.id, "artist": "Artist " + t.artist, "artist_id": t.artist,
		"album": "Album " + t.album, "album_id": t.album, "duration_ms": t.durMs}
}

func scrobbleEv(user string, t tr, at time.Time) map[string]any {
	return map[string]any{"type": "scrobble", "user": map[string]any{"username": user}, "track": t.json(),
		"player": map[string]any{"id": "p1", "name": "Feishin"}, "timestamp": at.UTC().Format(time.RFC3339)}
}

func nowPlayingEv(user string, t tr, at time.Time) map[string]any {
	return map[string]any{"type": "now_playing", "user": map[string]any{"username": user}, "track": t.json(),
		"player": map[string]any{"id": "p1", "name": "Feishin"}, "timestamp": at.UTC().Format(time.RFC3339), "position_ms": 1000}
}

func (e *env) scrobble(user string, t tr, at time.Time) {
	e.t.Helper()
	if n := e.ingest(scrobbleEv(user, t, at)); n != 1 {
		e.t.Fatalf("scrobble accepted %d", n)
	}
}

// feedIDs returns the ids of viewer's feed (optionally with a query).
func (e *env) feedIDs(viewer, query string) []string {
	e.t.Helper()
	var page struct {
		Items []struct {
			ID   string `json:"id"`
			Kind string `json:"kind"`
		} `json:"items"`
	}
	e.must(200, "GET", "/api/v1/feed"+query, viewer, nil, &page)
	ids := []string{}
	for _, it := range page.Items {
		ids = append(ids, it.ID)
	}
	return ids
}

func (e *env) liveUsers(viewer string) []string {
	e.t.Helper()
	var out struct {
		Items []struct {
			User struct {
				Username string `json:"username"`
			} `json:"user"`
		} `json:"items"`
	}
	e.must(200, "GET", "/api/v1/live", viewer, nil, &out)
	us := []string{}
	for _, it := range out.Items {
		us = append(us, it.User.Username)
	}
	return us
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// createCircle makes a custom circle owned by owner with the given members.
func (e *env) createCircle(owner, name string, members ...string) string {
	e.t.Helper()
	var c struct {
		ID string `json:"id"`
	}
	e.must(201, "POST", "/api/v1/circles", owner, map[string]any{"name": name}, &c)
	if len(members) > 0 {
		var inv struct {
			Code string `json:"code"`
		}
		e.must(201, "POST", "/api/v1/circles/"+c.ID+"/invites", owner, map[string]any{}, &inv)
		for _, m := range members {
			e.must(200, "POST", "/api/v1/invites/"+inv.Code+"/accept", m, nil, nil)
		}
	}
	return c.ID
}

func (e *env) leaveEveryone(users ...string) {
	for _, u := range users {
		e.must(204, "POST", "/api/v1/circles/everyone/leave", u, nil, nil)
	}
}

func (e *env) setSharing(user, circle string, flags map[string]bool) {
	e.t.Helper()
	e.must(200, "PUT", "/api/v1/circles/"+circle+"/sharing", user, flags, nil)
}

func fmtIDs(ids []string) string { return fmt.Sprintf("[%s]", strings.Join(ids, " ")) }

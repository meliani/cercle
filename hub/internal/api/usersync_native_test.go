package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/meliani/cercle/hub/internal/auth"
	"github.com/meliani/cercle/hub/internal/usersync"
)

// backdate makes users look inactive for the last 24 h: no recent API/ingest
// activity, no recently used or created session.
func (e *env) backdate(names ...string) {
	e.t.Helper()
	old := e.clock.Now().Add(-48 * time.Hour).UnixMilli()
	for _, n := range names {
		if _, err := e.srv.DB.W.Exec(`UPDATE users SET last_seen_at = ? WHERE username = ?`, old, n); err != nil {
			e.t.Fatal(err)
		}
		if _, err := e.srv.DB.W.Exec(`UPDATE sessions SET last_used_at = ?, created_at = ? WHERE username = ?`, old, old, n); err != nil {
			e.t.Fatal(err)
		}
	}
}

func (e *env) sync() usersync.Result {
	e.t.Helper()
	r, err := e.srv.Syncer.Sync(context.Background())
	if err != nil {
		e.t.Fatalf("sync: %v", err)
	}
	return r
}

func (e *env) displayName(user string) string {
	var n string
	_ = e.srv.DB.R.QueryRow(`SELECT display_name FROM users WHERE username = ?`, user).Scan(&n)
	return n
}

func (f *fakeNavidrome) loginCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logins
}

func (f *fakeNavidrome) set(fn func(f *fakeNavidrome)) {
	f.mu.Lock()
	fn(f)
	f.mu.Unlock()
}

func TestUserSyncCreatesDisablesReenables(t *testing.T) {
	e := newEnv(t, withSync, withExcluded("admin"))
	// The startup sync created everyone in Navidrome, before any login or play.
	for _, u := range []string{"alice", "bob", "carol", "dave"} {
		if !e.userExists(u) {
			t.Fatalf("%s should exist after the startup sync", u)
		}
	}
	if e.displayName("alice") != "Alice Liddell" || e.displayName("dave") != "dave" {
		t.Fatalf("display names from Navidrome name: %q %q", e.displayName("alice"), e.displayName("dave"))
	}
	if got := e.userNames("alice"); len(got) != 3 || !contains(got, "dave") {
		t.Fatalf("GET /users lists synced users sharing everyone: %v", got)
	}
	if e.everyoneCount("alice") != 4 {
		t.Fatal("everyone has the 4 synced users")
	}
	var alice struct {
		IsAdmin bool `json:"is_admin"`
	}
	e.must(200, "GET", "/api/v1/users/alice", "bob", nil, &alice)
	if !alice.IsAdmin {
		t.Fatal("is_admin comes from isAdmin")
	}
	// A customised display name survives syncs.
	e.must(200, "PATCH", "/api/v1/me", "carol", map[string]string{"display_name": "Caz"}, nil)

	// A new family member appears in Navidrome.
	e.nav.addUser("erin", false)
	if r := e.sync(); r.Created != 1 || r.Excluded != 1 {
		t.Fatalf("sync: %+v", r)
	}
	if !contains(e.userNames("bob"), "erin") || e.everyoneCount("bob") != 5 || e.displayName("erin") != "Erin" {
		t.Fatal("erin is in everyone, named, without ever logging in")
	}
	if e.displayName("carol") != "Caz" {
		t.Fatal("custom display name kept")
	}

	// bob was active, went quiet, then got deleted from Navidrome.
	e.scrobble("bob", track("t1", "al1", "ar1"), t0.Add(-10*time.Minute))
	e.ingest(nowPlayingEv("bob", track("t2", "al1", "ar1"), t0))
	fam := e.createCircle("alice", "Family", "bob")
	e.backdate("bob")
	if !e.sees("alice", "bob", "listened") || !contains(e.liveUsers("alice"), "bob") {
		t.Fatal("bob visible before removal")
	}
	e.nav.removeUser("bob")
	if r := e.sync(); r.Disabled != 1 || r.DisableSkipped != "" {
		t.Fatalf("sync after removal: %+v", r)
	}
	if contains(e.userNames("alice"), "bob") || e.everyoneCount("alice") != 4 {
		t.Fatal("disabled bob is excluded from lists and member counts")
	}
	var famDetail struct {
		MemberCount int `json:"member_count"`
	}
	e.must(200, "GET", "/api/v1/circles/"+fam, "alice", nil, &famDetail)
	if famDetail.MemberCount != 1 {
		t.Fatalf("custom circle count excludes bob: %d", famDetail.MemberCount)
	}
	if e.sees("alice", "bob", "listened") || contains(e.liveUsers("alice"), "bob") {
		t.Fatal("disabled bob is gone from feed and live")
	}
	if code := e.req("GET", "/api/v1/me", "bob", nil, nil); code != 401 {
		t.Fatalf("disabled bob's session revoked: %d", code)
	}
	if !e.userExists("bob") {
		t.Fatal("data is kept")
	}

	// bob comes back.
	e.nav.addUser("bob", false)
	if r := e.sync(); r.Reenabled != 1 {
		t.Fatalf("sync after re-add: %+v", r)
	}
	if !contains(e.userNames("alice"), "bob") || !e.sees("alice", "bob", "listened") {
		t.Fatal("re-enabled bob and his history are visible again")
	}
	e.must(200, "GET", "/api/v1/circles/"+fam, "alice", nil, &famDetail)
	if famDetail.MemberCount != 2 {
		t.Fatal("bob's memberships were kept")
	}
}

func TestUserSyncNativeAuth(t *testing.T) {
	e := newEnv(t, withSync, withExcluded("admin"))
	if n := e.nav.loginCount(); n != 1 {
		t.Fatalf("startup sync logs in once: %d", n)
	}
	e.sync()
	if n := e.nav.loginCount(); n != 1 {
		t.Fatalf("the JWT is cached: %d logins", n)
	}
	e.nav.expireJWTs()
	e.sync()
	if n := e.nav.loginCount(); n != 2 {
		t.Fatalf("401 → exactly one re-login: %d logins", n)
	}
	// A refreshed token sent back by Navidrome is adopted.
	e.nav.set(func(f *fakeNavidrome) { f.rotateToken = true })
	e.sync()
	e.nav.set(func(f *fakeNavidrome) {
		f.rotateToken = false
		// Only the rotated token stays valid.
		for tok := range f.jwts {
			if tok != "jwt-3-admin" {
				delete(f.jwts, tok)
			}
		}
	})
	e.sync()
	if n := e.nav.loginCount(); n != 2 {
		t.Fatalf("rotated token reused without logging in: %d", n)
	}

	// Bad credentials and non-admin accounts abort without changes.
	before := e.everyoneCount("alice")
	nav := e.srv.Syncer.Nav.(*auth.NativeNavidrome)
	nav.Password = "wrong"
	e.nav.expireJWTs()
	if _, err := e.srv.Syncer.Sync(context.Background()); !errors.Is(err, auth.ErrBadCredentials) {
		t.Fatalf("bad admin password: %v", err)
	}
	nav.User, nav.Password = "bob", "pw-bob"
	if _, err := e.srv.Syncer.Sync(context.Background()); !errors.Is(err, auth.ErrNotAdmin) {
		t.Fatalf("non-admin sync account: %v", err)
	}
	if e.everyoneCount("alice") != before {
		t.Fatal("a failed sync must not change anything")
	}
}

// The v0.2 production incident: the listing contained only the (excluded)
// admin, and the hub disabled the only real user.
func TestUserSyncProdFailureListingOnlyAdmin(t *testing.T) {
	e := newEnv(t, withSync, withExcluded("admin"))
	login(e, "bob") // the real, active user
	e.nav.set(func(f *fakeNavidrome) { f.listOnly = []string{"admin"} })

	r := e.sync()
	if r.Disabled != 0 || !strings.Contains(r.DisableSkipped, "active in the last 24h") {
		t.Fatalf("guard (a) must stop the disable step: %+v", r)
	}
	if !contains(e.userNames("alice"), "bob") {
		t.Fatal("bob still visible")
	}
	// Even with nobody active recently, a run never disables every active user…
	e.backdate("alice", "bob", "carol", "dave")
	r = e.sync()
	if r.Disabled != 0 || !strings.Contains(r.DisableSkipped, "all") {
		t.Fatalf("guard (d) must stop the disable step: %+v", r)
	}
	// …not even with the mass-disable override.
	e.srv.Syncer.AllowMassDisable = true
	if r = e.sync(); r.Disabled != 0 {
		t.Fatalf("guard (d) is not overridable: %+v", r)
	}
	var disabled int
	_ = e.srv.DB.R.QueryRow(`SELECT COUNT(*) FROM users WHERE disabled_at IS NOT NULL`).Scan(&disabled)
	if disabled != 0 {
		t.Fatalf("nobody may be disabled, got %d", disabled)
	}
}

func TestUserSyncGuards(t *testing.T) {
	e := newEnv(t, withSync, withExcluded("admin"))
	for _, u := range []string{"u1", "u2", "u3", "u4", "u5", "u6"} {
		e.nav.addUser(u, false)
	}
	e.sync() // 10 active users → limit max(1, 20%) = 2
	all := []string{"alice", "bob", "carol", "dave", "u1", "u2", "u3", "u4", "u5", "u6"}
	e.backdate(all...)

	// (b) three at once is too many…
	for _, u := range []string{"u1", "u2", "u3"} {
		e.nav.removeUser(u)
	}
	if r := e.sync(); r.Disabled != 0 || !strings.Contains(r.DisableSkipped, "would disable 3 of 10") {
		t.Fatalf("guard (b): %+v", r)
	}
	// …unless explicitly allowed.
	e.srv.Syncer.AllowMassDisable = true
	if r := e.sync(); r.Disabled != 3 {
		t.Fatalf("mass disable allowed: %+v", r)
	}
	e.srv.Syncer.AllowMassDisable = false

	// (c) a session used in the last 24 h protects its user even when every
	// other sign of activity looks old.
	e.token("u4")
	e.backdate("u4")
	if _, err := e.srv.DB.W.Exec(`UPDATE sessions SET last_used_at = ? WHERE username = 'u4'`, e.clock.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	e.nav.removeUser("u4")
	e.nav.removeUser("u5")
	if r := e.sync(); r.Protected != 1 || r.Disabled != 1 {
		t.Fatalf("guard (c): u4 protected, u5 disabled: %+v", r)
	}
	if code := e.req("GET", "/api/v1/me", "u4", nil, nil); code != 200 {
		t.Fatalf("u4 keeps working: %d", code)
	}

	// (a) a listing missing a recently active user disables nobody.
	e.must(200, "GET", "/api/v1/me", "alice", nil, nil) // alice just used the hub
	e.backdate("alice")
	if _, err := e.srv.DB.W.Exec(`UPDATE users SET last_seen_at = ? WHERE username = 'alice'`, e.clock.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	e.nav.removeUser("alice")
	e.nav.removeUser("u6")
	if r := e.sync(); r.Disabled != 0 || !strings.Contains(r.DisableSkipped, "active in the last 24h") {
		t.Fatalf("guard (a): %+v", r)
	}
}

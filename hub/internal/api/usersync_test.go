package api

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/meliani/cercle/hub/internal/config"
)

func withSync(c *config.Config) {
	c.NavidromeAdminUser, c.NavidromeAdminPassword = "admin", "pw-admin"
	c.UserSyncInterval = 15 * time.Minute
}

func withExcluded(names ...string) envOpt {
	return func(c *config.Config) {
		c.ExcludedUsers = map[string]bool{}
		for _, n := range names {
			c.ExcludedUsers[n] = true
		}
	}
}

func withDB(path string) envOpt { return func(c *config.Config) { c.DBPath = path } }

func (e *env) userNames(viewer string) []string {
	e.t.Helper()
	var out struct {
		Items []struct {
			Username string `json:"username"`
		} `json:"items"`
	}
	e.must(200, "GET", "/api/v1/users", viewer, nil, &out)
	var names []string
	for _, u := range out.Items {
		names = append(names, u.Username)
	}
	return names
}

func (e *env) everyoneCount(viewer string) int {
	e.t.Helper()
	var c struct {
		MemberCount int `json:"member_count"`
		Members     []struct {
			User struct {
				Username string `json:"username"`
			} `json:"user"`
		} `json:"members"`
	}
	e.must(200, "GET", "/api/v1/circles/everyone", viewer, nil, &c)
	if c.MemberCount != len(c.Members) {
		e.t.Fatalf("member_count %d != listed members %d", c.MemberCount, len(c.Members))
	}
	return c.MemberCount
}

func (e *env) userExists(name string) bool {
	var n int
	if err := e.srv.DB.R.QueryRow(`SELECT COUNT(*) FROM users WHERE username = ?`, name).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n > 0
}

func TestExcludedUsers(t *testing.T) {
	e := newEnv(t, withSync, withExcluded("admin"))
	if e.userExists("admin") {
		t.Fatal("sync never creates excluded users")
	}
	code, out := postJSON(t, e.ts.URL+"/hub/api/v1/auth/session", map[string]string{"username": "admin", "password": "pw-admin"})
	if code != 403 || out["error"].(map[string]any)["code"] != "excluded_user" {
		t.Fatalf("excluded login: %d %v", code, out)
	}
	if code, _ := postJSON(t, e.ts.URL+"/hub/api/v1/auth/session", map[string]string{"username": "admin", "password": "nope"}); code != 401 {
		t.Fatalf("wrong password is still 401 for excluded users: %d", code)
	}
	if n := e.ingest(scrobbleEv("ADMIN", track("t1", "al1", "ar1"), t0), nowPlayingEv("admin", track("t1", "al1", "ar1"), t0)); n != 2 {
		t.Fatalf("excluded events are accepted: %d", n)
	}
	var plays int
	_ = e.srv.DB.R.QueryRow(`SELECT COUNT(*) FROM plays`).Scan(&plays)
	if plays != 0 || e.userExists("ADMIN") || e.userExists("admin") || contains(e.liveUsers("alice"), "admin") {
		t.Fatal("excluded events are dropped")
	}
	resp, err := http.Get(e.ts.URL + "/rest/getNowPlaying.view?u=admin&p=pw-admin&f=json")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if e.userExists("admin") {
		t.Fatal("the Subsonic shim does not create excluded users")
	}
}

func TestExcludedExistingUserPurgedAtStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.db")
	e1 := newEnv(t, withDB(path))
	login(e1, "alice", "bob", "carol")
	club := e1.createCircle("carol", "Club", "bob", "alice")
	e1.must(200, "PUT", "/api/v1/circles/"+club+"/members/alice", "carol", map[string]string{"role": "admin"}, nil)
	solo := e1.createCircle("carol", "Solo")
	var rm roomResp
	e1.must(201, "POST", "/api/v1/circles/"+club+"/rooms", "carol", map[string]string{"name": "R"}, &rm)
	e1.must(200, "POST", "/api/v1/rooms/"+rm.ID+"/join", "bob", nil, nil)
	carolTok := e1.tokens["carol"]

	e2 := newEnv(t, withDB(path), withExcluded("carol"))
	var n int
	_ = e2.srv.DB.R.QueryRow(`SELECT COUNT(*) FROM memberships WHERE username = 'carol'`).Scan(&n)
	if n != 0 {
		t.Fatalf("carol removed from all circles, %d left", n)
	}
	var owner string
	_ = e2.srv.DB.R.QueryRow(`SELECT username FROM memberships WHERE circle_id = ? AND role = 'owner'`, club).Scan(&owner)
	if owner != "alice" {
		t.Fatalf("ownership goes to the senior admin: %q", owner)
	}
	_ = e2.srv.DB.R.QueryRow(`SELECT COUNT(*) FROM circles WHERE id = ?`, solo).Scan(&n)
	if n != 0 {
		t.Fatal("a circle carol was alone in is deleted")
	}
	e2.must(200, "GET", "/api/v1/rooms/"+rm.ID, "bob", nil, &rm)
	if rm.Host.Username != "bob" || len(rm.Listeners) != 1 {
		t.Fatalf("carol left the room: %+v", rm)
	}
	e2.tokens["carol"] = carolTok
	if code := e2.req("GET", "/api/v1/me", "carol", nil, nil); code != 401 {
		t.Fatalf("carol's sessions revoked: %d", code)
	}
	if contains(e2.userNames("bob"), "carol") {
		t.Fatal("carol no longer listed")
	}
}

func TestEveryoneNamingEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.db")
	name := func(e *env) (string, string) {
		var c struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		e.must(200, "GET", "/api/v1/circles/everyone", "alice", nil, &c)
		return c.Name, c.Description
	}
	e1 := newEnv(t, withDB(path), func(c *config.Config) {
		c.EveryoneName, c.EveryoneDescription = "Famille", "Toute la famille"
	})
	if n, d := name(e1); n != "Famille" || d != "Toute la famille" {
		t.Fatalf("env naming applied: %q %q", n, d)
	}
	e1.must(200, "PATCH", "/api/v1/circles/everyone", "alice", map[string]string{"name": "Nous"}, nil)

	e2 := newEnv(t, withDB(path))
	if n, d := name(e2); n != "Nous" || d != "Toute la famille" {
		t.Fatalf("unset env keeps the API rename: %q %q", n, d)
	}
	e3 := newEnv(t, withDB(path), func(c *config.Config) { c.EveryoneName = "Famille" })
	if n, _ := name(e3); n != "Famille" {
		t.Fatalf("set env wins at startup: %q", n)
	}
}

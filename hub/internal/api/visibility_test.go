package api

import (
	"testing"
	"time"
)

type feedEntry struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	User struct {
		Username string `json:"username"`
	} `json:"user"`
	PlayCount int `json:"play_count"`
	Track     struct {
		ID string `json:"id"`
	} `json:"track"`
}

func (e *env) feed(viewer, query string) []feedEntry {
	e.t.Helper()
	var page struct {
		Items []feedEntry `json:"items"`
	}
	e.must(200, "GET", "/api/v1/feed"+query, viewer, nil, &page)
	return page.Items
}

// sees reports whether viewer's feed has an item of kind by subject.
func (e *env) sees(viewer, subject, kind string) bool {
	e.t.Helper()
	for _, it := range e.feed(viewer, "") {
		if it.User.Username == subject && it.Kind == kind {
			return true
		}
	}
	return false
}

func (e *env) playsCount(viewer, subject string) (int, int) {
	e.t.Helper()
	var page struct {
		Items []any `json:"items"`
	}
	code := e.req("GET", "/api/v1/users/"+subject+"/plays", viewer, nil, &page)
	return code, len(page.Items)
}

func (e *env) statPlays(viewer, path string) (int, int64) {
	e.t.Helper()
	var st struct {
		Totals struct {
			Plays int64 `json:"plays"`
		} `json:"totals"`
	}
	code := e.req("GET", path, viewer, nil, &st)
	return code, st.Totals.Plays
}

func login(e *env, users ...string) {
	for _, u := range users {
		e.token(u)
	}
}

func TestEveryoneVisibleFromStart(t *testing.T) {
	e := newEnv(t)
	login(e, "alice", "bob")
	e.scrobble("alice", track("t1", "al1", "ar1"), t0.Add(-5*time.Minute))
	if !e.sees("bob", "alice", "listened") || !e.sees("bob", "alice", "discovery_artist") {
		t.Fatalf("bob should see alice via everyone: %+v", e.feed("bob", ""))
	}
	e.ingest(nowPlayingEv("alice", track("t2", "al1", "ar1"), t0))
	if !contains(e.liveUsers("bob"), "alice") {
		t.Fatal("bob should see alice live")
	}
	if !contains(e.liveUsers("alice"), "alice") {
		t.Fatal("alice should see herself live")
	}
}

func TestCircleMembershipRule(t *testing.T) {
	e := newEnv(t)
	login(e, "alice", "bob", "carol")
	e.leaveEveryone("alice", "bob", "carol")
	fam := e.createCircle("alice", "Family", "bob")
	e.scrobble("alice", track("t1", "al1", "ar1"), t0.Add(-5*time.Minute))
	e.ingest(nowPlayingEv("alice", track("t2", "al1", "ar1"), t0))

	if !e.sees("bob", "alice", "listened") {
		t.Fatal("bob shares Family with alice")
	}
	if e.sees("carol", "alice", "listened") || len(e.feed("carol", "")) != 0 {
		t.Fatal("carol shares no circle with alice")
	}
	if contains(e.liveUsers("carol"), "alice") {
		t.Fatal("carol must not see alice live")
	}
	if code := e.req("GET", "/api/v1/users/alice", "carol", nil, nil); code != 404 {
		t.Fatalf("carol GET /users/alice = %d, want 404", code)
	}
	if code, _ := e.playsCount("carol", "alice"); code != 404 {
		t.Fatalf("carol plays = %d, want 404", code)
	}
	// Circle filter.
	if got := e.feed("bob", "?circle="+fam); len(got) == 0 {
		t.Fatal("circle filter should include alice's items")
	}
	if code := e.req("GET", "/api/v1/feed?circle="+fam, "carol", nil, nil); code != 404 {
		t.Fatalf("non-member circle filter = %d, want 404", code)
	}
}

func TestSharingFlags(t *testing.T) {
	e := newEnv(t)
	login(e, "alice", "bob")
	e.leaveEveryone("alice", "bob")
	fam := e.createCircle("alice", "Family", "bob")
	e.scrobble("alice", track("t1", "al1", "ar1"), t0.Add(-5*time.Minute))
	e.ingest(nowPlayingEv("alice", track("t2", "al1", "ar1"), t0))

	// share_history off: listened gone, discovery stays, plays 403.
	e.setSharing("alice", fam, map[string]bool{"share_history": false})
	if e.sees("bob", "alice", "listened") {
		t.Fatal("share_history=false must hide listened items")
	}
	if !e.sees("bob", "alice", "discovery_artist") {
		t.Fatal("discoveries are governed by share_discoveries only")
	}
	if code, _ := e.playsCount("bob", "alice"); code != 403 {
		t.Fatalf("plays with share_history=false: %d, want 403", code)
	}
	if !e.sees("alice", "alice", "listened") {
		t.Fatal("alice always sees herself")
	}

	// share_discoveries off.
	e.setSharing("alice", fam, map[string]bool{"share_history": true, "share_discoveries": false})
	if e.sees("bob", "alice", "discovery_artist") || !e.sees("bob", "alice", "listened") {
		t.Fatal("share_discoveries=false must hide only discoveries")
	}

	// share_live off.
	if !contains(e.liveUsers("bob"), "alice") {
		t.Fatal("live visible by default")
	}
	e.setSharing("alice", fam, map[string]bool{"share_live": false})
	if contains(e.liveUsers("bob"), "alice") {
		t.Fatal("share_live=false must hide live")
	}

	// share_stats off.
	if code, n := e.statPlays("bob", "/api/v1/stats/users/alice?range=all"); code != 200 || n != 1 {
		t.Fatalf("stats visible: %d %d", code, n)
	}
	e.setSharing("alice", fam, map[string]bool{"share_stats": false})
	if code, _ := e.statPlays("bob", "/api/v1/stats/users/alice?range=all"); code != 403 {
		t.Fatalf("share_stats=false: %d, want 403", code)
	}
	// Circle stats exclude members not sharing stats.
	if _, n := e.statPlays("bob", "/api/v1/stats/circles/"+fam+"?range=all"); n != 0 {
		t.Fatalf("circle stats must exclude alice: %d", n)
	}

	// Another common circle with the flag on grants visibility again.
	band := e.createCircle("alice", "Band", "bob")
	_ = band
	if code, _ := e.statPlays("bob", "/api/v1/stats/users/alice?range=all"); code != 200 {
		t.Fatalf("a second circle sharing stats should grant access: %d", code)
	}
	if !contains(e.liveUsers("bob"), "alice") {
		t.Fatal("a second circle sharing live should grant access")
	}
}

func TestPauseMakesPlaysPrivateForever(t *testing.T) {
	e := newEnv(t)
	login(e, "alice", "bob")
	e.ingest(nowPlayingEv("alice", track("t0", "al0", "ar0"), t0))
	e.must(200, "POST", "/api/v1/me/pause", "alice", map[string]int{"minutes": 60}, nil)
	if contains(e.liveUsers("bob"), "alice") {
		t.Fatal("paused: live hidden")
	}
	e.scrobble("alice", track("t1", "al1", "ar1"), t0.Add(-1*time.Minute))
	if e.sees("bob", "alice", "listened") || e.sees("bob", "alice", "discovery_artist") {
		t.Fatal("plays while paused are private")
	}
	if !e.sees("alice", "alice", "listened") {
		t.Fatal("alice sees her private plays")
	}
	e.must(200, "POST", "/api/v1/me/pause", "alice", map[string]int{"minutes": 0}, nil)
	if !contains(e.liveUsers("bob"), "alice") {
		t.Fatal("resumed: live visible again")
	}
	if e.sees("bob", "alice", "listened") {
		t.Fatal("private plays never become visible")
	}
	e.scrobble("alice", track("t2", "al2", "ar2"), t0)
	if !e.sees("bob", "alice", "listened") {
		t.Fatal("new plays after resume are visible")
	}
	if _, n := e.statPlays("alice", "/api/v1/stats/me?range=all"); n != 2 {
		t.Fatalf("own stats include private plays: %d", n)
	}
	if _, n := e.statPlays("bob", "/api/v1/stats/users/alice?range=all"); n != 1 {
		t.Fatalf("others' stats exclude private plays: %d", n)
	}
	// Pause expiring by time.
	e.must(200, "POST", "/api/v1/me/pause", "alice", map[string]int{"minutes": 10}, nil)
	e.clock.Advance(11 * time.Minute)
	if !contains(e.liveUsers("bob"), "alice") {
		// live state may have expired; refresh it
		e.ingest(nowPlayingEv("alice", track("t3", "al3", "ar3"), e.clock.Now()))
		if !contains(e.liveUsers("bob"), "alice") {
			t.Fatal("pause should lapse after its end time")
		}
	}
}

func TestHistoryDelay(t *testing.T) {
	e := newEnv(t)
	login(e, "alice", "bob")
	e.must(200, "PUT", "/api/v1/me/settings", "alice", map[string]any{"history_delay_minutes": 30}, nil)
	e.scrobble("alice", track("t1", "al1", "ar1"), t0.Add(-4*time.Minute))
	e.ingest(nowPlayingEv("alice", track("t2", "al1", "ar1"), t0))
	if e.sees("bob", "alice", "listened") {
		t.Fatal("delayed play visible too early")
	}
	if _, n := e.playsCount("bob", "alice"); n != 0 {
		t.Fatal("delayed play visible in plays too early")
	}
	if !contains(e.liveUsers("bob"), "alice") {
		t.Fatal("live is unaffected by the history delay")
	}
	e.clock.Advance(27 * time.Minute)
	if !e.sees("bob", "alice", "listened") {
		t.Fatal("play older than the delay must be visible")
	}
	if _, n := e.playsCount("bob", "alice"); n != 1 {
		t.Fatal("plays visible after delay")
	}
}

func TestHiddenItems(t *testing.T) {
	e := newEnv(t)
	login(e, "alice", "bob")
	e.scrobble("alice", track("t1", "al1", "secret"), t0.Add(-30*time.Minute))
	e.scrobble("alice", track("t9", "al9", "ok"), t0.Add(-90*time.Minute))
	if !e.sees("bob", "alice", "listened") {
		t.Fatal("visible before hiding")
	}
	e.must(200, "POST", "/api/v1/me/hidden", "alice", map[string]string{"kind": "artist", "id": "secret", "label": "Secret"}, nil)
	for _, it := range e.feed("bob", "") {
		if it.Track.ID == "t1" {
			t.Fatal("hiding applies to past plays at query time")
		}
	}
	if _, n := e.playsCount("bob", "alice"); n != 1 {
		t.Fatalf("plays: hidden artist filtered, got %d", n)
	}
	e.scrobble("alice", track("t2", "al2", "secret"), t0.Add(-1*time.Minute))
	e.ingest(nowPlayingEv("alice", track("t3", "al2", "secret"), t0))
	if contains(e.liveUsers("bob"), "alice") {
		t.Fatal("live of a hidden artist is hidden")
	}
	e.must(200, "DELETE", "/api/v1/me/hidden/artist/secret", "alice", nil, nil)
	var seen1, seen2 bool
	for _, it := range e.feed("bob", "") {
		seen1 = seen1 || it.Track.ID == "t1"
		seen2 = seen2 || it.Track.ID == "t2"
	}
	if !seen1 {
		t.Fatal("unhiding restores past plays")
	}
	if seen2 {
		t.Fatal("plays recorded while hidden stay private")
	}
	var me struct {
		Settings struct {
			Hidden []any `json:"hidden"`
		} `json:"settings"`
	}
	e.must(200, "GET", "/api/v1/me", "alice", nil, &me)
	if len(me.Settings.Hidden) != 0 {
		t.Fatal("hidden list should be empty")
	}
}

func TestLeavingCircleHidesThePast(t *testing.T) {
	e := newEnv(t)
	login(e, "alice", "bob")
	e.leaveEveryone("alice", "bob")
	fam := e.createCircle("alice", "Family", "bob")
	e.scrobble("alice", track("t1", "al1", "ar1"), t0.Add(-5*time.Minute))
	if !e.sees("bob", "alice", "listened") {
		t.Fatal("visible while in the circle")
	}
	e.must(204, "POST", "/api/v1/circles/"+fam+"/leave", "bob", nil, nil)
	if len(e.feed("bob", "")) != 0 {
		t.Fatal("leaving a circle hides the past")
	}
	if code, _ := e.playsCount("bob", "alice"); code != 404 {
		t.Fatalf("plays after leaving: %d", code)
	}
	if !e.sees("alice", "alice", "listened") {
		t.Fatal("alice still sees herself")
	}
}

func TestEveryoneAutoJoinLeaveRejoin(t *testing.T) {
	e := newEnv(t)
	login(e, "alice", "dave")
	var circles struct {
		Items []struct {
			ID     string  `json:"id"`
			Kind   string  `json:"kind"`
			MyRole *string `json:"my_role"`
		} `json:"items"`
	}
	e.must(200, "GET", "/api/v1/circles", "dave", nil, &circles)
	if len(circles.Items) != 1 || circles.Items[0].ID != "everyone" || circles.Items[0].MyRole == nil || *circles.Items[0].MyRole != "member" {
		t.Fatalf("new user auto-joins everyone: %+v", circles.Items)
	}
	e.scrobble("dave", track("t1", "al1", "ar1"), t0.Add(-5*time.Minute))
	if !e.sees("alice", "dave", "listened") {
		t.Fatal("alice sees dave via everyone")
	}
	e.leaveEveryone("dave")
	if e.sees("alice", "dave", "listened") {
		t.Fatal("after leaving everyone dave is invisible")
	}
	e.must(200, "GET", "/api/v1/circles", "dave", nil, &circles)
	if len(circles.Items) != 1 || circles.Items[0].MyRole != nil {
		t.Fatalf("everyone still listed with my_role null: %+v", circles.Items)
	}
	// Logging in again does not silently re-add.
	delete(e.tokens, "dave")
	login(e, "dave")
	if e.sees("alice", "dave", "listened") {
		t.Fatal("re-login must not re-join everyone")
	}
	e.must(200, "POST", "/api/v1/circles/everyone/join", "dave", nil, nil)
	if !e.sees("alice", "dave", "listened") {
		t.Fatal("rejoin restores visibility (including the past)")
	}
	// everyone cannot be deleted, has no invites, owner-less.
	if code := e.req("DELETE", "/api/v1/circles/everyone", "alice", nil, nil); code != 403 {
		t.Fatalf("delete everyone = %d", code)
	}
	// Only server admins may rename it (alice is a Navidrome admin in the fake).
	e.must(200, "PATCH", "/api/v1/circles/everyone", "alice", map[string]string{"name": "All of us"}, nil)
	if code := e.req("PATCH", "/api/v1/circles/everyone", "dave", map[string]string{"name": "x"}, nil); code != 403 {
		t.Fatalf("non-admin rename = %d", code)
	}
}

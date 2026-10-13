package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestIngestAuthAndBatch(t *testing.T) {
	e := newEnv(t)
	r, _ := http.NewRequest("POST", e.ts.URL+"/hub/ingest/v1/events", bytes.NewReader([]byte(`{}`)))
	r.Header.Set("Authorization", "Bearer wrong-secret-wrong-secret-wrong-secret")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("bad secret: %d", resp.StatusCode)
	}

	n := e.ingest(
		scrobbleEv("alice", track("t1", "al1", "ar1"), t0.Add(-time.Hour)),
		map[string]any{"type": "future_thing", "user": map[string]any{"username": "alice"}},
		map[string]any{"type": "scrobble", "user": map[string]any{}, "track": track("t2", "a", "b").json()},
	)
	if n != 2 {
		t.Fatalf("accepted %d, want 2 (unknown type counted, missing username skipped)", n)
	}

	var many []map[string]any
	for i := 0; i < 101; i++ {
		many = append(many, scrobbleEv("alice", track("x", "y", "z"), t0))
	}
	b, _ := json.Marshal(map[string]any{"events": many})
	r, _ = http.NewRequest("POST", e.ts.URL+"/hub/ingest/v1/events", bytes.NewReader(b))
	r.Header.Set("Authorization", "Bearer "+testSecret)
	resp, err = http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf(">100 events: %d, want 400", resp.StatusCode)
	}
}

func TestIngestDedupe(t *testing.T) {
	e := newEnv(t)
	at := t0.Add(-time.Hour)
	e.scrobble("alice", track("t1", "al1", "ar1"), at)
	e.scrobble("alice", track("t1", "al1", "ar1"), at.Add(3*time.Second))
	e.scrobble("alice", track("t1", "al1", "ar1"), at.Add(-4*time.Second))
	if _, n := e.playsCount("alice", "alice"); n != 1 {
		t.Fatalf("dedupe within ±5 s: %d plays", n)
	}
	e.scrobble("alice", track("t1", "al1", "ar1"), at.Add(6*time.Second))
	if _, n := e.playsCount("alice", "alice"); n != 2 {
		t.Fatalf("6 s apart is a new play: %d", n)
	}
}

func TestFeedGrouping(t *testing.T) {
	e := newEnv(t)
	base := t0.Add(-5 * time.Hour)
	e.scrobble("alice", track("t1", "al1", "ar1"), base)
	e.scrobble("alice", track("t2", "al1", "ar1"), base.Add(4*time.Minute))
	items := listened(e.feed("alice", ""))
	if len(items) != 1 || items[0].PlayCount != 2 || items[0].Track.ID != "t2" {
		t.Fatalf("same album, small gap → one item with the last track: %+v", items)
	}
	firstID := items[0].ID
	// t2 ended at base+4m+200s; 31 min later is a new group.
	e.scrobble("alice", track("t3", "al1", "ar1"), base.Add(4*time.Minute+200*time.Second+31*time.Minute))
	items = listened(e.feed("alice", ""))
	if len(items) != 2 {
		t.Fatalf("30-min gap → new item: %+v", items)
	}
	// album change → new item, even with no gap
	e.scrobble("alice", track("t4", "al2", "ar1"), base.Add(4*time.Minute+200*time.Second+31*time.Minute+200*time.Second))
	items = listened(e.feed("alice", ""))
	if len(items) != 3 {
		t.Fatalf("album change → new item: %+v", items)
	}
	if items[2].ID != firstID {
		t.Fatalf("stable id = first play id: %s vs %s", items[2].ID, firstID)
	}
	// 29 min gap within the same album still groups.
	e.scrobble("alice", track("t5", "al2", "ar1"), base.Add(4*time.Minute+200*time.Second+31*time.Minute+400*time.Second+29*time.Minute))
	items = listened(e.feed("alice", ""))
	if len(items) != 3 || items[0].PlayCount != 2 {
		t.Fatalf("29-min gap groups: %+v", items)
	}
	// Pagination.
	var page struct {
		Items []feedEntry `json:"items"`
		Next  *string     `json:"next"`
	}
	e.must(200, "GET", "/api/v1/feed?limit=2", "alice", nil, &page)
	if len(page.Items) != 2 || page.Next == nil {
		t.Fatalf("page 1: %+v", page)
	}
	seen := map[string]bool{page.Items[0].ID: true, page.Items[1].ID: true}
	total := 2
	for page.Next != nil {
		cur := *page.Next
		page.Items, page.Next = nil, nil
		e.must(200, "GET", "/api/v1/feed?limit=2&before="+cur, "alice", nil, &page)
		for _, it := range page.Items {
			if seen[it.ID] {
				t.Fatalf("duplicate across pages: %s", it.ID)
			}
			seen[it.ID] = true
			total++
		}
	}
	if total != len(e.feed("alice", "?limit=200")) {
		t.Fatalf("pagination lost items: %d", total)
	}
}

func listened(items []feedEntry) []feedEntry {
	var out []feedEntry
	for _, it := range items {
		if it.Kind == "listened" {
			out = append(out, it)
		}
	}
	return out
}

func kinds(items []feedEntry) map[string]int {
	m := map[string]int{}
	for _, it := range items {
		m[it.Kind]++
	}
	return m
}

func TestDiscoveries(t *testing.T) {
	e := newEnv(t)
	base := t0.Add(-10 * time.Hour)
	e.scrobble("alice", track("t1", "al1", "ar1"), base)
	if k := kinds(e.feed("alice", "")); k["discovery_artist"] != 1 || k["discovery_album"] != 0 {
		t.Fatalf("first play of an artist → discovery_artist: %v", k)
	}
	e.scrobble("alice", track("t2", "al2", "ar1"), base.Add(time.Hour))
	if k := kinds(e.feed("alice", "")); k["discovery_artist"] != 1 || k["discovery_album"] != 1 {
		t.Fatalf("new album of a known artist → discovery_album: %v", k)
	}
	e.scrobble("alice", track("t3", "al1", "ar1"), base.Add(2*time.Hour))
	if k := kinds(e.feed("alice", "")); k["discovery_artist"] != 1 || k["discovery_album"] != 1 {
		t.Fatalf("known album → no discovery: %v", k)
	}
	// Missing ids: stored, never a discovery.
	e.ingest(map[string]any{"type": "scrobble", "user": map[string]any{"username": "alice"},
		"track":     map[string]any{"id": "t9", "title": "No ids", "artist": "Someone", "album": "Something", "duration_ms": 1000},
		"timestamp": base.Add(3 * time.Hour).Format(time.RFC3339)})
	if k := kinds(e.feed("alice", "")); k["discovery_artist"] != 1 || k["discovery_album"] != 1 {
		t.Fatalf("empty ids never produce discoveries: %v", k)
	}
	if _, n := e.playsCount("alice", "alice"); n != 4 {
		t.Fatalf("play without ids is stored: %d", n)
	}
}

func TestPlaybackStatesAndLiveExpiry(t *testing.T) {
	e := newEnv(t)
	login(e, "alice", "bob")
	state := func(s string, pos int64) map[string]any {
		return map[string]any{"type": "playback_state", "user": map[string]any{"username": "alice"},
			"track": track("t1", "al1", "ar1").json(), "state": s, "position_ms": pos, "timestamp": e.clock.Now().Format(time.RFC3339)}
	}
	liveState := func() (string, int64) {
		var out struct {
			Items []struct {
				State      string `json:"state"`
				PositionMs int64  `json:"position_ms"`
			} `json:"items"`
		}
		e.must(200, "GET", "/api/v1/live", "bob", nil, &out)
		if len(out.Items) == 0 {
			return "", 0
		}
		return out.Items[0].State, out.Items[0].PositionMs
	}
	e.ingest(state("starting", 0))
	if s, _ := liveState(); s != "playing" {
		t.Fatalf("starting ≈ playing, got %q", s)
	}
	e.ingest(state("paused", 42000))
	if s, p := liveState(); s != "paused" || p != 42000 {
		t.Fatalf("paused: %q %d", s, p)
	}
	e.ingest(state("expired", 0))
	if s, _ := liveState(); s != "" {
		t.Fatalf("expired ≈ stopped clears live, got %q", s)
	}
	e.ingest(nowPlayingEv("alice", track("t2", "al1", "ar1"), e.clock.Now()))
	e.ingest(state("stopped", 0))
	if s, _ := liveState(); s != "" {
		t.Fatal("stopped clears live")
	}
	e.ingest(nowPlayingEv("alice", track("t2", "al1", "ar1"), e.clock.Now()))
	e.clock.Advance(200*time.Second + 59*time.Second)
	if s, _ := liveState(); s != "playing" {
		t.Fatal("still live before duration + 60 s")
	}
	e.clock.Advance(2 * time.Second)
	if s, _ := liveState(); s != "" {
		t.Fatal("expired after duration + 60 s without update")
	}
}

func TestIngestUserMetadata(t *testing.T) {
	e := newEnv(t)
	e.ingest(map[string]any{"type": "now_playing", "user": map[string]any{"username": "carol", "name": "Carol C."},
		"track": track("t1", "al1", "ar1").json()})
	var me struct {
		DisplayName string `json:"display_name"`
	}
	e.must(200, "GET", "/api/v1/me", "carol", nil, &me)
	if me.DisplayName != "Carol C." {
		t.Fatalf("display name from plugin: %q", me.DisplayName)
	}
	e.must(200, "PATCH", "/api/v1/me", "carol", map[string]string{"display_name": "Caz"}, &me)
	e.ingest(map[string]any{"type": "now_playing", "user": map[string]any{"username": "carol", "name": "Carol C."},
		"track": track("t1", "al1", "ar1").json()})
	e.must(200, "GET", "/api/v1/me", "carol", nil, &me)
	if me.DisplayName != "Caz" {
		t.Fatalf("custom display name kept: %q", me.DisplayName)
	}
}

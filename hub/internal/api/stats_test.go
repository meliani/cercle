package api

import (
	"math"
	"testing"
	"time"

	"github.com/meliani/cercle/hub/internal/config"
)

type statsResp struct {
	Range  string `json:"range"`
	Totals struct {
		Plays           int64 `json:"plays"`
		Ms              int64 `json:"ms"`
		ActiveDays      int   `json:"active_days"`
		DistinctArtists int64 `json:"distinct_artists"`
		DistinctAlbums  int64 `json:"distinct_albums"`
	} `json:"totals"`
	TopArtists []statEntry `json:"top_artists"`
	TopAlbums  []statEntry `json:"top_albums"`
	TopTracks  []statEntry `json:"top_tracks"`
	ByHour     []int64     `json:"by_hour"`
	ByWeekday  []int64     `json:"by_weekday"`
	ByDay      []struct {
		Date  string `json:"date"`
		Plays int64  `json:"plays"`
	} `json:"by_day"`
}

type statEntry struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Plays int64  `json:"plays"`
	Ms    int64  `json:"ms"`
}

func TestStatsFixture(t *testing.T) {
	plus2 := time.FixedZone("UTC+2", 2*3600)
	e := newEnv(t, func(c *config.Config) { c.Location = plus2 })
	login(e, "alice", "bob")
	// t0 = 2026-03-10 12:00 UTC (a Tuesday). Local hours are UTC+2.
	day := time.Date(2026, 3, 10, 8, 0, 0, 0, time.UTC) // 10:00 local
	e.scrobble("alice", track("t1", "al1", "ar1"), day)
	e.scrobble("alice", track("t1", "al1", "ar1"), day.Add(10*time.Minute))
	e.scrobble("alice", track("t2", "al1", "ar1"), day.Add(20*time.Minute))
	e.scrobble("alice", track("t1", "al1", "ar1"), day.Add(-24*time.Hour)) // Monday
	e.scrobble("alice", track("t3", "al2", "ar2"), day.Add(-23*time.Hour))
	e.scrobble("alice", track("t3", "al2", "ar2"), day.Add(-20*24*time.Hour)) // outside 7d
	e.scrobble("alice", track("t4", "al4", "ar4"), time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC))

	var st statsResp
	e.must(200, "GET", "/api/v1/stats/me?range=7d", "alice", nil, &st)
	if st.Range != "7d" || st.Totals.Plays != 5 || st.Totals.Ms != 5*200000 || st.Totals.DistinctArtists != 2 ||
		st.Totals.DistinctAlbums != 2 || st.Totals.ActiveDays != 2 {
		t.Fatalf("7d totals: %+v", st.Totals)
	}
	if len(st.TopArtists) != 2 || st.TopArtists[0].ID != "ar1" || st.TopArtists[0].Plays != 4 || st.TopArtists[1].Plays != 1 {
		t.Fatalf("top artists: %+v", st.TopArtists)
	}
	if st.TopTracks[0].ID != "t1" || st.TopTracks[0].Plays != 3 {
		t.Fatalf("top tracks: %+v", st.TopTracks)
	}
	if st.TopAlbums[0].ID != "al1" || st.TopAlbums[0].Plays != 4 {
		t.Fatalf("top albums: %+v", st.TopAlbums)
	}
	if st.ByHour[10] != 4 || st.ByHour[11] != 1 || len(st.ByHour) != 24 {
		t.Fatalf("by_hour in configured TZ: %v", st.ByHour)
	}
	if len(st.ByWeekday) != 7 || st.ByWeekday[1] != 3 || st.ByWeekday[0] != 2 {
		t.Fatalf("by_weekday (0=Mon): %v", st.ByWeekday)
	}
	var sum int64
	for _, d := range st.ByDay {
		sum += d.Plays
	}
	if sum != 5 || len(st.ByDay) < 7 {
		t.Fatalf("by_day: %+v", st.ByDay)
	}

	e.must(200, "GET", "/api/v1/stats/me?range=30d", "alice", nil, &st)
	if st.Totals.Plays != 6 {
		t.Fatalf("30d: %d", st.Totals.Plays)
	}
	e.must(200, "GET", "/api/v1/stats/me?range=all", "alice", nil, &st)
	if st.Totals.Plays != 7 {
		t.Fatalf("all: %d", st.Totals.Plays)
	}
	e.must(200, "GET", "/api/v1/stats/me?range=2025", "alice", nil, &st)
	if st.Totals.Plays != 1 || st.TopArtists[0].ID != "ar4" {
		t.Fatalf("2025: %+v", st)
	}
	e.must(200, "GET", "/api/v1/stats/me?range=year", "alice", nil, &st)
	if st.Totals.Plays != 6 {
		t.Fatalf("year: %d", st.Totals.Plays)
	}
	if code := e.req("GET", "/api/v1/stats/me?range=forever", "alice", nil, nil); code != 400 {
		t.Fatalf("bad range: %d", code)
	}

	// Others see the same public plays.
	e.must(200, "GET", "/api/v1/stats/users/alice?range=all", "bob", nil, &st)
	if st.Totals.Plays != 7 {
		t.Fatalf("bob sees alice: %d", st.Totals.Plays)
	}

	// Compare: alice {ar1, ar2, ar4} vs bob {ar1, ar3}.
	e.scrobble("bob", track("t1", "al1", "ar1"), day)
	e.scrobble("bob", track("t9", "al9", "ar3"), day.Add(time.Hour))
	var cmp struct {
		CommonArtists []statEntry `json:"common_artists"`
		CommonAlbums  []statEntry `json:"common_albums"`
		Score         float64     `json:"score"`
	}
	e.must(200, "GET", "/api/v1/stats/compare/bob?range=all", "alice", nil, &cmp)
	if len(cmp.CommonArtists) != 1 || cmp.CommonArtists[0].ID != "ar1" || cmp.CommonArtists[0].Plays != 5 ||
		len(cmp.CommonAlbums) != 1 || math.Abs(cmp.Score-0.25) > 1e-9 {
		t.Fatalf("compare: %+v", cmp)
	}

	// Circle stats (everyone): both members share stats.
	var cs struct {
		statsResp
		Members []struct {
			User struct {
				Username string `json:"username"`
			} `json:"user"`
			Plays int64 `json:"plays"`
		} `json:"members"`
	}
	e.must(200, "GET", "/api/v1/stats/circles/everyone?range=all", "alice", nil, &cs)
	if cs.Totals.Plays != 9 || len(cs.Members) != 2 || cs.Members[0].User.Username != "alice" || cs.Members[0].Plays != 7 {
		t.Fatalf("circle stats: %+v", cs)
	}

	var wr struct {
		statsResp
		FirstPlay *struct {
			Track struct {
				ID string `json:"id"`
			} `json:"track"`
		} `json:"first_play"`
		TopMonth   *string `json:"top_month"`
		LongestDay *struct {
			Date string `json:"date"`
			Ms   int64  `json:"ms"`
		} `json:"longest_day"`
		NewArtists int `json:"new_artists"`
	}
	e.must(200, "GET", "/api/v1/wrapped/2026", "alice", nil, &wr)
	if wr.Totals.Plays != 6 || wr.FirstPlay == nil || wr.FirstPlay.Track.ID != "t3" || wr.TopMonth == nil || *wr.TopMonth != "2026-03" ||
		wr.LongestDay == nil || wr.LongestDay.Date != "2026-03-10" || wr.LongestDay.Ms != 600000 || wr.NewArtists != 2 {
		t.Fatalf("wrapped: %+v first=%+v month=%v day=%+v", wr.Totals, wr.FirstPlay, wr.TopMonth, wr.LongestDay)
	}
	e.must(200, "GET", "/api/v1/wrapped/2026?user=bob", "alice", nil, &wr)
	if wr.Totals.Plays != 2 || wr.NewArtists != 2 {
		t.Fatalf("wrapped for bob: %+v", wr.Totals)
	}
}

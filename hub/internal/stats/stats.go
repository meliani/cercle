// Package stats computes listening statistics over plays. Aggregates are
// SQL; hour/weekday/day buckets are done in Go in the configured time zone.
package stats

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/meliani/cercle/hub/internal/db"
	"github.com/meliani/cercle/hub/internal/model"
	"github.com/meliani/cercle/hub/internal/visibility"
)

// TopN is the length of each top list.
const TopN = 10

// Service computes stats.
type Service struct {
	DB  *db.DB
	Vis *visibility.V
	Loc *time.Location
	Now func() time.Time
}

// Scope selects plays: all plays of Self (the owner sees everything), plus
// the publicly visible plays (not private, not hidden, past the delay) of
// Public. Callers check membership-level permission (share_stats) first.
type Scope struct {
	Self   string
	Public []string
}

func (s *Service) scopeSQL(sc Scope) (string, []any) {
	var parts []string
	var args []any
	if sc.Self != "" {
		parts = append(parts, `p.user_id = ?`)
		args = append(args, sc.Self)
	}
	if len(sc.Public) > 0 {
		cond, cargs := s.Vis.PublicPlayCond("p")
		parts = append(parts, `(p.user_id IN (`+db.Placeholders(len(sc.Public))+`) AND `+cond+`)`)
		for _, u := range sc.Public {
			args = append(args, u)
		}
		args = append(args, cargs...)
	}
	if len(parts) == 0 {
		return `0`, nil
	}
	return `(` + strings.Join(parts, ` OR `) + `)`, args
}

// Window is a resolved stats range [From, To) in unix ms.
type Window struct {
	Label    string
	From, To int64
	FillDays bool
}

// ParseRange resolves 7d|30d|year|all|YYYY.
func (s *Service) ParseRange(r string) (Window, error) {
	now := s.Now().In(s.Loc)
	if r == "" {
		r = "30d"
	}
	switch r {
	case "7d", "30d":
		days := 7
		if r == "30d" {
			days = 30
		}
		return Window{Label: r, From: now.Add(-time.Duration(days) * 24 * time.Hour).UnixMilli(), To: now.UnixMilli() + 1, FillDays: true}, nil
	case "year":
		y := time.Date(now.Year(), 1, 1, 0, 0, 0, 0, s.Loc)
		return Window{Label: r, From: y.UnixMilli(), To: y.AddDate(1, 0, 0).UnixMilli(), FillDays: true}, nil
	case "all":
		return Window{Label: r, From: 0, To: now.UnixMilli() + 1}, nil
	}
	if len(r) == 4 {
		if y, err := strconv.Atoi(r); err == nil && y >= 1970 && y <= 9999 {
			t := time.Date(y, 1, 1, 0, 0, 0, 0, s.Loc)
			return Window{Label: r, From: t.UnixMilli(), To: t.AddDate(1, 0, 0).UnixMilli(), FillDays: true}, nil
		}
	}
	return Window{}, fmt.Errorf("%w: range must be 7d, 30d, year, all or YYYY", model.ErrInvalid)
}

const (
	artistKey = `CASE WHEN p.artist_id <> '' THEN p.artist_id ELSE 'name:' || lower(p.artist) END`
	albumKey  = `CASE WHEN p.album_id <> '' THEN p.album_id ELSE 'name:' || lower(p.album) END`
	trackKey  = `p.track_id`
)

func (s *Service) where(sc Scope, w Window) (string, []any) {
	cond, args := s.scopeSQL(sc)
	return ` FROM plays p WHERE ` + cond + ` AND p.played_at >= ? AND p.played_at < ?`, append(args, w.From, w.To)
}

// Compute returns Stats for the scope and window.
func (s *Service) Compute(ctx context.Context, sc Scope, w Window) (model.Stats, error) {
	st := model.Stats{Range: w.Label, TopArtists: []model.StatEntry{}, TopAlbums: []model.StatEntry{},
		TopTracks: []model.StatEntry{}, ByDay: []model.DayCount{}}
	from, args := s.where(sc, w)
	err := s.DB.R.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(p.duration_ms), 0),
		COUNT(DISTINCT `+artistKey+`), COUNT(DISTINCT `+albumKey+`)`+from, args...).
		Scan(&st.Totals.Plays, &st.Totals.Ms, &st.Totals.DistinctArtists, &st.Totals.DistinctAlbums)
	if err != nil {
		return st, err
	}
	if st.TopArtists, err = s.top(ctx, "artist", sc, w, TopN); err != nil {
		return st, err
	}
	if st.TopAlbums, err = s.top(ctx, "album", sc, w, TopN); err != nil {
		return st, err
	}
	if st.TopTracks, err = s.top(ctx, "track", sc, w, TopN); err != nil {
		return st, err
	}
	days := map[string]int64{}
	rows, err := s.DB.R.QueryContext(ctx, `SELECT p.played_at`+from, args...)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var ms int64
		if err := rows.Scan(&ms); err != nil {
			return st, err
		}
		t := time.UnixMilli(ms).In(s.Loc)
		st.ByHour[t.Hour()]++
		st.ByWeekday[(int(t.Weekday())+6)%7]++ // 0 = Monday
		days[t.Format("2006-01-02")]++
	}
	if err := rows.Err(); err != nil {
		return st, err
	}
	st.Totals.ActiveDays = len(days)
	st.ByDay = s.byDay(days, w)
	return st, nil
}

func (s *Service) byDay(days map[string]int64, w Window) []model.DayCount {
	out := []model.DayCount{}
	if !w.FillDays {
		keys := make([]string, 0, len(days))
		for k := range days {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out = append(out, model.DayCount{Date: k, Plays: days[k]})
		}
		return out
	}
	end := time.UnixMilli(min(w.To-1, s.Now().UnixMilli())).In(s.Loc)
	d := time.UnixMilli(w.From).In(s.Loc)
	d = time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, s.Loc)
	for i := 0; i < 370 && !d.After(end); i++ {
		k := d.Format("2006-01-02")
		out = append(out, model.DayCount{Date: k, Plays: days[k]})
		d = d.AddDate(0, 0, 1)
	}
	return out
}

// top returns the top entries of kind artist|album|track.
func (s *Service) top(ctx context.Context, kind string, sc Scope, w Window, n int) ([]model.StatEntry, error) {
	from, args := s.where(sc, w)
	var q string
	switch kind {
	case "artist":
		q = `SELECT ` + artistKey + ` AS k, MAX(p.artist_id), MAX(p.artist), '', '', COUNT(*), COALESCE(SUM(p.duration_ms), 0)`
	case "album":
		q = `SELECT ` + albumKey + ` AS k, MAX(p.album_id), MAX(p.album),
			MAX(CASE WHEN p.album_artist <> '' THEN p.album_artist ELSE p.artist END), MAX(p.album_id), COUNT(*), COALESCE(SUM(p.duration_ms), 0)`
	case "track":
		q = `SELECT ` + trackKey + ` AS k, MAX(p.track_id), MAX(p.title), MAX(p.artist), MAX(p.album_id), COUNT(*), COALESCE(SUM(p.duration_ms), 0)`
	}
	q += from + ` GROUP BY k ORDER BY 6 DESC, 7 DESC, 3 ASC`
	if n > 0 {
		q += ` LIMIT ` + strconv.Itoa(n)
	}
	rows, err := s.DB.R.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.StatEntry{}
	for rows.Next() {
		var key string
		var e model.StatEntry
		if err := rows.Scan(&key, &e.ID, &e.Name, &e.Artist, &e.AlbumID, &e.Plays, &e.Ms); err != nil {
			return nil, err
		}
		if e.ID == "" {
			e.ID = key
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PerUser returns plays/ms per user for the scope (circle stats).
func (s *Service) PerUser(ctx context.Context, sc Scope, w Window) (map[string][2]int64, error) {
	from, args := s.where(sc, w)
	rows, err := s.DB.R.QueryContext(ctx, `SELECT p.user_id, COUNT(*), COALESCE(SUM(p.duration_ms), 0)`+from+` GROUP BY p.user_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][2]int64{}
	for rows.Next() {
		var u string
		var a, b int64
		if err := rows.Scan(&u, &a, &b); err != nil {
			return nil, err
		}
		out[u] = [2]int64{a, b}
	}
	return out, rows.Err()
}

// Compare computes common artists/albums between two scopes and the Jaccard
// index over artists.
func (s *Service) Compare(ctx context.Context, a, b Scope, w Window) (model.Compare, error) {
	out := model.Compare{CommonArtists: []model.StatEntry{}, CommonAlbums: []model.StatEntry{}}
	for _, kind := range []string{"artist", "album"} {
		la, err := s.top(ctx, kind, a, w, 0)
		if err != nil {
			return out, err
		}
		lb, err := s.top(ctx, kind, b, w, 0)
		if err != nil {
			return out, err
		}
		mb := map[string]model.StatEntry{}
		for _, e := range lb {
			mb[e.ID] = e
		}
		var common []model.StatEntry
		for _, e := range la {
			if o, ok := mb[e.ID]; ok {
				e.Plays += o.Plays
				e.Ms += o.Ms
				common = append(common, e)
			}
		}
		sort.SliceStable(common, func(i, j int) bool { return common[i].Plays > common[j].Plays })
		if kind == "artist" {
			union := len(la) + len(lb) - len(common)
			if union > 0 {
				out.Score = float64(len(common)) / float64(union)
			}
		}
		if len(common) > 25 {
			common = common[:25]
		}
		if common == nil {
			common = []model.StatEntry{}
		}
		if kind == "artist" {
			out.CommonArtists = common
		} else {
			out.CommonAlbums = common
		}
	}
	return out, nil
}

// Wrapped computes the yearly recap.
func (s *Service) Wrapped(ctx context.Context, sc Scope, year int) (model.Wrapped, error) {
	w, err := s.ParseRange(fmt.Sprintf("%04d", year))
	if err != nil {
		return model.Wrapped{}, err
	}
	st, err := s.Compute(ctx, sc, w)
	if err != nil {
		return model.Wrapped{}, err
	}
	out := model.Wrapped{Stats: st}
	from, args := s.where(sc, w)

	var fp model.FirstPlay
	var playedAt int64
	err = s.DB.R.QueryRowContext(ctx, `SELECT p.track_id, p.title, p.artist, p.artist_id, p.album, p.album_id, p.duration_ms, p.played_at`+
		from+` ORDER BY p.played_at, p.id LIMIT 1`, args...).
		Scan(&fp.Track.ID, &fp.Track.Title, &fp.Track.Artist, &fp.Track.ArtistID, &fp.Track.Album, &fp.Track.AlbumID, &fp.Track.DurationMs, &playedAt)
	switch {
	case err == nil:
		fp.PlayedAt = model.TS(playedAt)
		out.FirstPlay = &fp
	case err != sql.ErrNoRows:
		return out, err
	}

	months := map[string]int64{}
	dayMs := map[string]int64{}
	rows, err := s.DB.R.QueryContext(ctx, `SELECT p.played_at, p.duration_ms`+from, args...)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var at, ms int64
		if err := rows.Scan(&at, &ms); err != nil {
			rows.Close()
			return out, err
		}
		t := time.UnixMilli(at).In(s.Loc)
		months[t.Format("2006-01")]++
		dayMs[t.Format("2006-01-02")] += ms
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	if m := argmax(months); m != "" {
		out.TopMonth = &m
	}
	if d := argmax(dayMs); d != "" {
		out.LongestDay = &model.DayMs{Date: d, Ms: dayMs[d]}
	}

	cond, cargs := s.scopeSQL(sc)
	err = s.DB.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT `+artistKey+` AS k, MIN(p.played_at) AS first
		FROM plays p WHERE `+cond+` GROUP BY k) WHERE first >= ? AND first < ?`, append(cargs, w.From, w.To)...).Scan(&out.NewArtists)
	return out, err
}

// argmax returns the key with the highest value (ties: smallest key).
func argmax(m map[string]int64) string {
	best, bestV := "", int64(-1)
	for k, v := range m {
		if v > bestV || (v == bestV && k < best) {
			best, bestV = k, v
		}
	}
	return best
}

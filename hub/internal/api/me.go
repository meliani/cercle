package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/meliani/cercle/hub/internal/model"
	"github.com/meliani/cercle/hub/internal/visibility"
)

const (
	maxDelayMinutes = 30 * 24 * 60  // 30 days
	maxPauseMinutes = 365 * 24 * 60 // one year
)

func (s *Server) getMe(w http.ResponseWriter, r *http.Request, me string) {
	m, err := s.Users.Me(r.Context(), me)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) patchMe(w http.ResponseWriter, r *http.Request, me string) {
	var req struct {
		DisplayName *string `json:"display_name"`
	}
	if !s.decode(w, r, &req) {
		return
	}
	if req.DisplayName != nil {
		name := strings.TrimSpace(*req.DisplayName)
		if n := utf8.RuneCountInString(name); n < 1 || n > 64 {
			writeError(w, http.StatusBadRequest, "bad_request", "display_name must be 1..64 characters")
			return
		}
		if _, err := s.DB.W.ExecContext(r.Context(), `UPDATE users SET display_name = ?, display_name_custom = 1 WHERE username = ?`, name, me); err != nil {
			s.fail(w, err)
			return
		}
	}
	s.getMe(w, r, me)
}

func (s *Server) writeSettings(w http.ResponseWriter, r *http.Request, me string) {
	// Privacy changed: re-evaluate who may see my live state right now.
	s.Live.Refresh(r.Context(), me)
	st, err := s.Users.Settings(r.Context(), me)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request, me string) {
	var req map[string]json.RawMessage
	if !s.decode(w, r, &req) {
		return
	}
	if raw, ok := req["paused_until"]; ok {
		var v *string
		if err := json.Unmarshal(raw, &v); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "paused_until must be an RFC 3339 string or null")
			return
		}
		var until any
		if v != nil {
			ms, err := model.ParseTS(*v)
			if err != nil {
				writeError(w, http.StatusBadRequest, "bad_request", "paused_until must be an RFC 3339 string or null")
				return
			}
			if ms > s.Now().UnixMilli() {
				until = ms
			}
		}
		if _, err := s.DB.W.ExecContext(r.Context(), `UPDATE users SET paused_until = ? WHERE username = ?`, until, me); err != nil {
			s.fail(w, err)
			return
		}
	}
	if raw, ok := req["history_delay_minutes"]; ok {
		var n int
		if err := json.Unmarshal(raw, &n); err != nil || n < 0 || n > maxDelayMinutes {
			writeError(w, http.StatusBadRequest, "bad_request", "history_delay_minutes must be an integer 0..43200")
			return
		}
		if _, err := s.DB.W.ExecContext(r.Context(), `UPDATE users SET history_delay_minutes = ? WHERE username = ?`, n, me); err != nil {
			s.fail(w, err)
			return
		}
	}
	s.writeSettings(w, r, me)
}

func (s *Server) pause(w http.ResponseWriter, r *http.Request, me string) {
	var req struct {
		Minutes *int `json:"minutes"`
	}
	if !s.decode(w, r, &req) {
		return
	}
	if req.Minutes == nil || *req.Minutes < 0 || *req.Minutes > maxPauseMinutes {
		writeError(w, http.StatusBadRequest, "bad_request", "minutes must be an integer 0..525600 (0 = resume)")
		return
	}
	var until any
	if *req.Minutes > 0 {
		until = s.Now().Add(time.Duration(*req.Minutes) * time.Minute).UnixMilli()
	}
	if _, err := s.DB.W.ExecContext(r.Context(), `UPDATE users SET paused_until = ? WHERE username = ?`, until, me); err != nil {
		s.fail(w, err)
		return
	}
	s.writeSettings(w, r, me)
}

func validKind(k string) bool { return k == "artist" || k == "album" || k == "track" }

func (s *Server) addHidden(w http.ResponseWriter, r *http.Request, me string) {
	var h model.Hidden
	if !s.decode(w, r, &h) {
		return
	}
	h.ID = strings.TrimSpace(h.ID)
	if !validKind(h.Kind) || h.ID == "" || len(h.ID) > 256 || utf8.RuneCountInString(h.Label) > 512 {
		writeError(w, http.StatusBadRequest, "bad_request", "kind must be artist|album|track, id 1..256 bytes, label ≤ 512 characters")
		return
	}
	if _, err := s.DB.W.ExecContext(r.Context(), `INSERT INTO hidden_items (username, kind, item_id, label, created_at)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT(username, kind, item_id) DO UPDATE SET label = excluded.label`,
		me, h.Kind, h.ID, h.Label, s.Now().UnixMilli()); err != nil {
		s.fail(w, err)
		return
	}
	s.writeSettings(w, r, me)
}

func (s *Server) delHidden(w http.ResponseWriter, r *http.Request, me string) {
	kind, id := r.PathValue("kind"), r.PathValue("id")
	if !validKind(kind) {
		writeError(w, http.StatusBadRequest, "bad_request", "kind must be artist|album|track")
		return
	}
	if _, err := s.DB.W.ExecContext(r.Context(), `DELETE FROM hidden_items WHERE username = ? AND kind = ? AND item_id = ?`, me, kind, id); err != nil {
		s.fail(w, err)
		return
	}
	s.writeSettings(w, r, me)
}

// --- users ------------------------------------------------------------

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request, me string) {
	rows, err := s.DB.R.QueryContext(r.Context(), `SELECT DISTINCT b.username FROM memberships a
		JOIN memberships b ON b.circle_id = a.circle_id WHERE a.username = ? AND b.username <> ?`, me, me)
	if err != nil {
		s.fail(w, err)
		return
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			s.fail(w, err)
			return
		}
		names = append(names, n)
	}
	rows.Close()
	us, err := s.Users.GetMany(r.Context(), names)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]model.User, 0, len(us))
	for _, u := range us {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := strings.ToLower(out[i].DisplayName), strings.ToLower(out[j].DisplayName)
		if a != b {
			return a < b
		}
		return out[i].Username < out[j].Username
	})
	writeJSON(w, http.StatusOK, model.List[model.User]{Items: out})
}

// requireKnown returns 404 unless subject is me or shares a circle with me.
func (s *Server) requireKnown(r *http.Request, me, subject string) error {
	ok, err := s.Vis.CommonCircle(r.Context(), me, subject)
	if err != nil {
		return err
	}
	if !ok {
		return model.ErrNotFound
	}
	if _, err := s.Users.Get(r.Context(), subject); err != nil {
		return err
	}
	return nil
}

// requireShares returns 404 for strangers and 403 when subject does not
// share category c with me through any common circle.
func (s *Server) requireShares(r *http.Request, me, subject string, c visibility.Category) error {
	if err := s.requireKnown(r, me, subject); err != nil {
		return err
	}
	ok, err := s.Vis.SharesWith(r.Context(), me, subject, c, "")
	if err != nil {
		return err
	}
	if !ok {
		return newErr(http.StatusForbidden, "forbidden", subject+" does not share "+string(c)+" with you")
	}
	return nil
}

func (s *Server) getUser(w http.ResponseWriter, r *http.Request, me string) {
	subject := r.PathValue("username")
	if err := s.requireKnown(r, me, subject); err != nil {
		s.fail(w, err)
		return
	}
	u, err := s.Users.Get(r.Context(), subject)
	if err != nil {
		s.fail(w, err)
		return
	}
	circles, err := s.circlesWhere(r, me, `c.id IN (SELECT a.circle_id FROM memberships a JOIN memberships b
		ON b.circle_id = a.circle_id WHERE a.username = ? AND b.username = ?)`, me, subject)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		model.User
		CommonCircles []model.Circle `json:"common_circles"`
	}{u, circles})
}

func (s *Server) userPlays(w http.ResponseWriter, r *http.Request, me string) {
	subject := r.PathValue("username")
	n, err := limit(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.requireShares(r, me, subject, visibility.History); err != nil {
		s.fail(w, err)
		return
	}
	q := `SELECT p.id, p.track_id, p.title, p.artist, p.artist_id, p.album, p.album_id, p.duration_ms, p.played_at, p.player_name
		FROM plays p WHERE p.user_id = ?`
	args := []any{subject}
	if subject != me {
		cond, cargs := s.Vis.PublicPlayCond("p")
		q += ` AND ` + cond
		args = append(args, cargs...)
	}
	if b := r.URL.Query().Get("before"); b != "" {
		at, id, err := decodePlayCursor(b)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid cursor")
			return
		}
		q += ` AND (p.played_at < ? OR (p.played_at = ? AND p.id < ?))`
		args = append(args, at, at, id)
	}
	q += ` ORDER BY p.played_at DESC, p.id DESC LIMIT ?`
	args = append(args, n+1)
	rows, err := s.DB.R.QueryContext(r.Context(), q, args...)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer rows.Close()
	type row struct {
		id, at int64
		p      model.Play
	}
	var list []row
	for rows.Next() {
		var x row
		if err := rows.Scan(&x.id, &x.p.Track.ID, &x.p.Track.Title, &x.p.Track.Artist, &x.p.Track.ArtistID,
			&x.p.Track.Album, &x.p.Track.AlbumID, &x.p.Track.DurationMs, &x.at, &x.p.PlayerName); err != nil {
			s.fail(w, err)
			return
		}
		x.p.ID = itoa(x.id)
		x.p.PlayedAt = model.TS(x.at)
		list = append(list, x)
	}
	if err := rows.Err(); err != nil {
		s.fail(w, err)
		return
	}
	page := model.Page[model.Play]{Items: []model.Play{}}
	if len(list) > n {
		list = list[:n]
		last := list[len(list)-1]
		c := encodePlayCursor(last.at, last.id)
		page.Next = &c
	}
	for _, x := range list {
		page.Items = append(page.Items, x.p)
	}
	writeJSON(w, http.StatusOK, page)
}

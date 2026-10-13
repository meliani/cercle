package api

import (
	"net/http"
	"sort"
	"strconv"

	"github.com/meliani/cercle/hub/internal/model"
	"github.com/meliani/cercle/hub/internal/stats"
	"github.com/meliani/cercle/hub/internal/visibility"
)

// scopeFor returns the stats scope for viewing subject's stats as me.
func (s *Server) scopeFor(r *http.Request, me, subject string) (stats.Scope, error) {
	if subject == "" || subject == me {
		return stats.Scope{Self: me}, nil
	}
	if err := s.requireShares(r, me, subject, visibility.Stats); err != nil {
		return stats.Scope{}, err
	}
	return stats.Scope{Public: []string{subject}}, nil
}

func (s *Server) window(w http.ResponseWriter, r *http.Request) (stats.Window, bool) {
	win, err := s.Stats.ParseRange(r.URL.Query().Get("range"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return win, false
	}
	return win, true
}

func (s *Server) statsFor(w http.ResponseWriter, r *http.Request, me, subject string) {
	win, ok := s.window(w, r)
	if !ok {
		return
	}
	sc, err := s.scopeFor(r, me, subject)
	if err != nil {
		s.fail(w, err)
		return
	}
	st, err := s.Stats.Compute(r.Context(), sc, win)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) statsMe(w http.ResponseWriter, r *http.Request, me string) { s.statsFor(w, r, me, me) }

func (s *Server) statsUser(w http.ResponseWriter, r *http.Request, me string) {
	s.statsFor(w, r, me, r.PathValue("username"))
}

func (s *Server) statsCircle(w http.ResponseWriter, r *http.Request, me string) {
	win, ok := s.window(w, r)
	if !ok {
		return
	}
	c, err := s.memberCircle(r.Context(), me, r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	rows, err := s.DB.R.QueryContext(r.Context(), `SELECT username FROM active_memberships WHERE circle_id = ? AND share_stats = 1`, c.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	var sharing []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			rows.Close()
			s.fail(w, err)
			return
		}
		sharing = append(sharing, u)
	}
	rows.Close()
	out := model.CircleStats{Members: []model.MemberStat{}}
	// Circle-wide numbers use everyone's *public* plays, the viewer
	// included, so every member sees the same totals.
	sc := stats.Scope{Public: sharing}
	if out.Stats, err = s.Stats.Compute(r.Context(), sc, win); err != nil {
		s.fail(w, err)
		return
	}
	per, err := s.Stats.PerUser(r.Context(), sc, win)
	if err != nil {
		s.fail(w, err)
		return
	}
	us, err := s.Users.GetMany(r.Context(), sharing)
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, u := range sharing {
		out.Members = append(out.Members, model.MemberStat{User: us[u], Plays: per[u][0], Ms: per[u][1]})
	}
	sort.SliceStable(out.Members, func(i, j int) bool {
		if out.Members[i].Plays != out.Members[j].Plays {
			return out.Members[i].Plays > out.Members[j].Plays
		}
		return out.Members[i].User.Username < out.Members[j].User.Username
	})
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) statsCompare(w http.ResponseWriter, r *http.Request, me string) {
	win, ok := s.window(w, r)
	if !ok {
		return
	}
	other := r.PathValue("username")
	if other == me {
		writeError(w, http.StatusBadRequest, "bad_request", "compare with someone else")
		return
	}
	sc, err := s.scopeFor(r, me, other)
	if err != nil {
		s.fail(w, err)
		return
	}
	cmp, err := s.Stats.Compare(r.Context(), stats.Scope{Self: me}, sc, win)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cmp)
}

func (s *Server) wrapped(w http.ResponseWriter, r *http.Request, me string) {
	year, err := strconv.Atoi(r.PathValue("year"))
	if err != nil || year < 1970 || year > 9999 {
		writeError(w, http.StatusBadRequest, "bad_request", "year must be YYYY")
		return
	}
	sc, err := s.scopeFor(r, me, r.URL.Query().Get("user"))
	if err != nil {
		s.fail(w, err)
		return
	}
	out, err := s.Stats.Wrapped(r.Context(), sc, year)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

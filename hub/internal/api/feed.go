package api

import (
	"net/http"

	"github.com/meliani/cercle/hub/internal/feed"
	"github.com/meliani/cercle/hub/internal/model"
)

func (s *Server) getLive(w http.ResponseWriter, r *http.Request, me string) {
	items, err := s.Live.VisibleTo(r.Context(), me)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, model.List[model.Live]{Items: items})
}

func (s *Server) getFeed(w http.ResponseWriter, r *http.Request, me string) {
	n, err := limit(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	q := r.URL.Query()
	o := feed.ListOpts{Circle: q.Get("circle"), User: q.Get("user"), Limit: n, Before: q.Get("before")}
	if o.Circle != "" {
		if _, err := s.memberCircle(r.Context(), me, o.Circle); err != nil {
			s.fail(w, err)
			return
		}
	}
	if o.User != "" {
		if err := s.requireKnown(r, me, o.User); err != nil {
			s.fail(w, err)
			return
		}
	}
	page, err := s.Feed.List(r.Context(), me, o)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) listComments(w http.ResponseWriter, r *http.Request, me string) {
	list, err := s.Feed.Comments(r.Context(), me, r.PathValue("item_id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, model.List[model.Comment]{Items: list})
}

type commentReq struct {
	Body string `json:"body"`
}

func (s *Server) addComment(w http.ResponseWriter, r *http.Request, me string) {
	var req commentReq
	if !s.decode(w, r, &req) {
		return
	}
	if _, ok := feed.ValidBody(req.Body); !ok {
		writeError(w, http.StatusBadRequest, "bad_request", "body must be 1..2000 characters")
		return
	}
	c, err := s.Feed.AddComment(r.Context(), me, r.PathValue("item_id"), req.Body)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) editComment(w http.ResponseWriter, r *http.Request, me string) {
	var req commentReq
	if !s.decode(w, r, &req) {
		return
	}
	if _, ok := feed.ValidBody(req.Body); !ok {
		writeError(w, http.StatusBadRequest, "bad_request", "body must be 1..2000 characters")
		return
	}
	c, err := s.Feed.EditComment(r.Context(), me, r.PathValue("id"), req.Body)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) deleteComment(w http.ResponseWriter, r *http.Request, me string) {
	if err := s.Feed.DeleteComment(r.Context(), me, r.PathValue("id")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) reaction(w http.ResponseWriter, r *http.Request, me string, add bool) {
	emoji := r.PathValue("emoji")
	if !feed.ValidEmoji(emoji) {
		writeError(w, http.StatusBadRequest, "bad_request", "emoji must be a single emoji")
		return
	}
	it, err := s.Feed.React(r.Context(), me, r.PathValue("item_id"), emoji, add)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, it)
}

func (s *Server) react(w http.ResponseWriter, r *http.Request, me string) { s.reaction(w, r, me, true) }
func (s *Server) unreact(w http.ResponseWriter, r *http.Request, me string) {
	s.reaction(w, r, me, false)
}

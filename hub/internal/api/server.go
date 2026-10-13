// Package api wires every service into the HTTP surface of docs/CONTRACT.md
// §3–§5b: REST /api/v1, the WebSocket, ingest, and the Subsonic shim.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/meliani/cercle/hub/internal/auth"
	"github.com/meliani/cercle/hub/internal/config"
	"github.com/meliani/cercle/hub/internal/db"
	"github.com/meliani/cercle/hub/internal/feed"
	"github.com/meliani/cercle/hub/internal/ingest"
	"github.com/meliani/cercle/hub/internal/live"
	"github.com/meliani/cercle/hub/internal/model"
	"github.com/meliani/cercle/hub/internal/rooms"
	"github.com/meliani/cercle/hub/internal/stats"
	"github.com/meliani/cercle/hub/internal/subsonic"
	"github.com/meliani/cercle/hub/internal/users"
	"github.com/meliani/cercle/hub/internal/usersync"
	"github.com/meliani/cercle/hub/internal/visibility"
	"github.com/meliani/cercle/hub/internal/ws"
)

// Server holds every service.
type Server struct {
	Cfg      *config.Config
	DB       *db.DB
	Users    *users.Service
	Vis      *visibility.V
	Sessions *auth.Sessions
	Nav      auth.Navidrome
	Limiter  *auth.FailLimiter
	Live     *live.Service
	Feed     *feed.Service
	Stats    *stats.Service
	Rooms    *rooms.Manager
	Ingest   *ingest.Service
	Hub      *ws.Hub
	Subsonic *subsonic.Handler
	// Syncer is nil unless Navidrome user sync is configured.
	Syncer *usersync.Syncer
	// SyncDone is closed after the first user sync (immediately when disabled).
	SyncDone chan struct{}
	Version  string
	Now      func() time.Time
	Log      *slog.Logger

	ensuredMu sync.Mutex
	ensured   map[string]bool // trusted-header users already created
}

// New wires the services. now may be nil (time.Now).
func New(cfg *config.Config, d *db.DB, nav auth.Navidrome, version string, log *slog.Logger, now func() time.Time) *Server {
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	hub := ws.New(log)
	vis := visibility.New(d.R, now)
	us := users.New(d, now)
	us.Excluded = cfg.IsExcluded
	lv := live.New(vis, us, hub, now, log)
	fd := &feed.Service{DB: d, Vis: vis, Users: us, Pub: hub, Now: now}
	s := &Server{
		Cfg: cfg, DB: d, Users: us, Vis: vis, Nav: nav, Version: version, Now: now, Log: log,
		Sessions: &auth.Sessions{DB: d, Now: now},
		Limiter:  auth.NewFailLimiter(10, time.Minute, now),
		Live:     lv,
		Feed:     fd,
		Stats:    &stats.Service{DB: d, Vis: vis, Loc: cfg.Location, Now: now},
		Rooms:    rooms.New(d, now),
		Ingest:   &ingest.Service{Secret: []byte(cfg.IngestSecret), DB: d, Users: us, Live: lv, Feed: fd, Now: now, Log: log},
		Hub:      hub,
		SyncDone: make(chan struct{}),
		ensured:  map[string]bool{},
	}
	if lister, ok := nav.(auth.UserLister); ok && cfg.UserSyncEnabled() {
		s.Syncer = &usersync.Syncer{Nav: lister, AdminUser: cfg.NavidromeAdminUser, AdminPassword: cfg.NavidromeAdminPassword,
			DB: d, Users: us, Vis: vis, Live: lv, Now: now, Log: log}
	}
	s.Subsonic = &subsonic.Handler{Nav: nav, Users: us, Live: lv, Limiter: s.Limiter,
		TrustedProxies: cfg.TrustedProxies, Version: version, Now: now, Log: log}
	hub.Authenticate = s.authenticateWS
	hub.Hello = func(ctx context.Context, u string) (any, error) {
		me, err := s.Users.Me(ctx, u)
		return map[string]any{"user": me}, err
	}
	hub.CanSubscribe = func(roomID, u string) bool {
		r, ok := s.Rooms.Get(roomID)
		return ok && r.IsListener(u)
	}
	hub.RoomState = func(roomID string) (any, bool) {
		r, ok := s.Rooms.Get(roomID)
		if !ok {
			return nil, false
		}
		return map[string]any{"room_id": r.ID, "state": r.State.Wire()}, true
	}
	hub.AllowOrigin = s.wsOriginAllowed
	return s
}

// Start loads persisted state, applies startup configuration (everyone
// circle naming, excluded users) and runs background loops until ctx ends.
func (s *Server) Start(ctx context.Context) error {
	if err := s.Rooms.Load(ctx); err != nil {
		return err
	}
	if err := s.applyEveryoneNaming(ctx); err != nil {
		return err
	}
	if err := s.purgeExcluded(ctx); err != nil {
		return err
	}
	if s.Syncer != nil {
		go s.Syncer.Run(ctx, s.Cfg.UserSyncInterval, s.SyncDone)
	} else {
		close(s.SyncDone)
	}
	go s.Live.Run(ctx, 5*time.Second)
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := s.Sessions.Prune(ctx); err != nil {
					s.Log.Warn("prune sessions", "err", err)
				}
			}
		}
	}()
	return nil
}

type handler func(w http.ResponseWriter, r *http.Request, me string)

// Handler returns the root HTTP handler (base path, CORS, routes).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	route := func(pattern string, h handler) { mux.HandleFunc(pattern, s.authed(h)) }

	mux.HandleFunc("GET /health", s.health)
	mux.Handle("POST /ingest/v1/events", s.Ingest)
	for _, p := range []string{"/rest/getNowPlaying", "/rest/getNowPlaying.view"} {
		mux.Handle("GET "+p, s.Subsonic)
		mux.Handle("POST "+p, s.Subsonic)
	}

	mux.HandleFunc("POST /api/v1/auth/session", s.login)
	route("DELETE /api/v1/auth/session", s.logout)
	mux.Handle("GET /api/v1/ws", s.Hub)

	route("GET /api/v1/me", s.getMe)
	route("PATCH /api/v1/me", s.patchMe)
	route("PUT /api/v1/me/settings", s.putSettings)
	route("POST /api/v1/me/pause", s.pause)
	route("POST /api/v1/me/hidden", s.addHidden)
	route("DELETE /api/v1/me/hidden/{kind}/{id}", s.delHidden)

	route("GET /api/v1/users", s.listUsers)
	route("GET /api/v1/users/{username}", s.getUser)
	route("GET /api/v1/users/{username}/plays", s.userPlays)

	route("GET /api/v1/circles", s.listCircles)
	route("POST /api/v1/circles", s.createCircle)
	route("GET /api/v1/circles/{id}", s.getCircle)
	route("PATCH /api/v1/circles/{id}", s.patchCircle)
	route("DELETE /api/v1/circles/{id}", s.deleteCircle)
	route("POST /api/v1/circles/{id}/join", s.joinCircle)
	route("POST /api/v1/circles/{id}/leave", s.leaveCircle)
	route("PUT /api/v1/circles/{id}/sharing", s.putSharing)
	route("PUT /api/v1/circles/{id}/members/{username}", s.putMember)
	route("DELETE /api/v1/circles/{id}/members/{username}", s.deleteMember)
	route("POST /api/v1/circles/{id}/invites", s.createInvite)
	route("GET /api/v1/circles/{id}/invites", s.listInvites)
	route("DELETE /api/v1/circles/{id}/invites/{code}", s.deleteInvite)
	route("GET /api/v1/invites/{code}", s.getInvite)
	route("POST /api/v1/invites/{code}/accept", s.acceptInvite)

	route("GET /api/v1/live", s.getLive)
	route("GET /api/v1/feed", s.getFeed)
	route("GET /api/v1/feed/{item_id}/comments", s.listComments)
	route("POST /api/v1/feed/{item_id}/comments", s.addComment)
	route("PATCH /api/v1/comments/{id}", s.editComment)
	route("DELETE /api/v1/comments/{id}", s.deleteComment)
	route("PUT /api/v1/feed/{item_id}/reactions/{emoji}", s.react)
	route("DELETE /api/v1/feed/{item_id}/reactions/{emoji}", s.unreact)

	route("GET /api/v1/stats/me", s.statsMe)
	route("GET /api/v1/stats/users/{username}", s.statsUser)
	route("GET /api/v1/stats/circles/{id}", s.statsCircle)
	route("GET /api/v1/stats/compare/{username}", s.statsCompare)
	route("GET /api/v1/wrapped/{year}", s.wrapped)

	route("GET /api/v1/circles/{id}/rooms", s.listRooms)
	route("POST /api/v1/circles/{id}/rooms", s.createRoom)
	route("GET /api/v1/rooms/{id}", s.getRoom)
	route("POST /api/v1/rooms/{id}/join", s.joinRoom)
	route("POST /api/v1/rooms/{id}/leave", s.leaveRoom)
	route("PUT /api/v1/rooms/{id}/state", s.putRoomState)
	route("PUT /api/v1/rooms/{id}/host", s.putRoomHost)
	route("POST /api/v1/rooms/{id}/requests", s.addRoomRequest)
	route("DELETE /api/v1/rooms/{id}/requests/{rid}", s.deleteRoomRequest)
	route("DELETE /api/v1/rooms/{id}", s.deleteRoom)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
	})

	base := s.Cfg.BasePath
	var root http.Handler = mux
	if base != "" {
		stripped := http.StripPrefix(base, mux)
		root = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := r.URL.Path
			switch {
			case p == base || strings.HasPrefix(p, base+"/"):
				stripped.ServeHTTP(w, r)
			case strings.HasPrefix(p, "/rest/getNowPlaying"), p == "/health":
				// The Subsonic shim is also served at the root because the
				// reverse proxy may forward the original Navidrome path.
				mux.ServeHTTP(w, r)
			default:
				writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
			}
		})
	}
	return s.cors(s.recoverer(root))
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.Log.Error("panic", "path", r.URL.Path, "panic", v)
				writeError(w, http.StatusInternalServerError, "internal", "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) originAllowed(origin string) bool {
	origin = strings.TrimRight(origin, "/")
	for _, o := range s.Cfg.CORSOrigins {
		if o == "*" || strings.EqualFold(strings.TrimRight(o, "/"), origin) {
			return true
		}
	}
	return false
}

func (s *Server) wsOriginAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" || s.originAllowed(origin) {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		preflight := r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""
		if origin != "" {
			w.Header().Add("Vary", "Origin")
			if s.originAllowed(origin) {
				h := w.Header()
				h.Set("Access-Control-Allow-Origin", origin)
				if preflight {
					h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
					h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
					h.Set("Access-Control-Max-Age", "600")
				}
			}
		}
		if preflight {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ok := s.DB.R.PingContext(r.Context()) == nil
	status := http.StatusOK
	if !ok {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{"ok": ok, "version": s.Version})
}

// --- auth -------------------------------------------------------------

// resolve finds the user for a request: trusted proxy header first, then
// the session token (bearer, or ?token= for the WebSocket).
func (s *Server) resolve(ctx context.Context, r *http.Request, allowQuery bool) (string, error) {
	if s.Cfg.TrustedHeader != "" && auth.FromTrustedProxy(r, s.Cfg.TrustedProxies) {
		if u := strings.TrimSpace(r.Header.Get(s.Cfg.TrustedHeader)); u != "" {
			if s.Users.IsExcluded(u) {
				return "", users.ErrExcluded
			}
			s.ensuredMu.Lock()
			done := s.ensured[u]
			s.ensuredMu.Unlock()
			if !done {
				if err := s.Users.EnsureTx(ctx, u, users.EnsureOpts{}); err != nil {
					return "", err
				}
				s.ensuredMu.Lock()
				s.ensured[u] = true
				s.ensuredMu.Unlock()
			}
			return u, nil
		}
	}
	tok := auth.BearerToken(r)
	if tok == "" && allowQuery {
		tok = r.URL.Query().Get("token")
	}
	u, err := s.Sessions.Lookup(ctx, tok)
	if err == nil && s.Users.IsExcluded(u) {
		return "", users.ErrExcluded
	}
	return u, err
}

func (s *Server) authenticateWS(ctx context.Context, r *http.Request) (string, error) {
	u, err := s.resolve(ctx, r, true)
	if err == nil {
		s.Users.Touch(ctx, u)
	}
	return u, err
}

func (s *Server) authed(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, err := s.resolve(r.Context(), r, false)
		if errors.Is(err, users.ErrExcluded) {
			writeError(w, http.StatusForbidden, "excluded_user", "this account is excluded from Cercle")
			return
		}
		if err != nil {
			if !errors.Is(err, auth.ErrNoSession) {
				s.Log.Error("auth", "err", err)
			}
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid session token")
			return
		}
		s.Users.Touch(r.Context(), u)
		h(w, r.WithContext(auth.WithUser(r.Context(), u)), u)
	}
}

type loginReq struct {
	Username string `json:"username"`
	Token    string `json:"token"`
	Salt     string `json:"salt"`
	Password string `json:"password"`
	APIKey   string `json:"api_key"`
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if !s.decode(w, r, &req) {
		return
	}
	c := auth.Creds{Username: strings.TrimSpace(req.Username), Token: req.Token, Salt: req.Salt,
		Password: req.Password, APIKey: req.APIKey}
	if !c.Valid() || c.Username == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "username plus exactly one of token+salt, password or api_key is required")
		return
	}
	ip := auth.ClientIP(r, s.Cfg.TrustedProxies)
	if s.Limiter.Blocked(ip) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many failed logins, try again in a minute")
		return
	}
	id, err := s.Nav.Validate(r.Context(), c)
	switch {
	case errors.Is(err, auth.ErrBadCredentials):
		s.Limiter.Fail(ip)
		writeError(w, http.StatusUnauthorized, "unauthorized", "Navidrome rejected the credentials")
		return
	case errors.Is(err, auth.ErrUnavailable):
		s.Log.Warn("navidrome unavailable", "err", err)
		writeError(w, http.StatusBadGateway, "bad_gateway", "Navidrome is unavailable")
		return
	case err != nil:
		s.fail(w, err)
		return
	}
	if s.Users.IsExcluded(id.Username) {
		writeError(w, http.StatusForbidden, "excluded_user", "this account is excluded from Cercle")
		return
	}
	if err := s.Users.EnsureTx(r.Context(), id.Username, users.EnsureOpts{IsAdmin: &id.IsAdmin}); err != nil {
		s.fail(w, err)
		return
	}
	tok, err := s.Sessions.Create(r.Context(), id.Username)
	if err != nil {
		s.fail(w, err)
		return
	}
	u, err := s.Users.Get(r.Context(), id.Username)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": tok, "user": u})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, _ string) {
	if tok := auth.BearerToken(r); tok != "" {
		if err := s.Sessions.Delete(r.Context(), tok); err != nil {
			s.fail(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- helpers ----------------------------------------------------------

// apiError carries an explicit status and code.
type apiError struct {
	status  int
	code    string
	message string
}

func (e *apiError) Error() string { return e.message }

func newErr(status int, code, msg string) error { return &apiError{status, code, msg} }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

// fail maps service errors to HTTP responses.
func (s *Server) fail(w http.ResponseWriter, err error) {
	var ae *apiError
	switch {
	case errors.As(err, &ae):
		writeError(w, ae.status, ae.code, ae.message)
	case errors.Is(err, users.ErrExcluded):
		writeError(w, http.StatusForbidden, "excluded_user", "this account is excluded from Cercle")
	case errors.Is(err, model.ErrNotFound), errors.Is(err, sql.ErrNoRows):
		writeError(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, model.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden", "not allowed")
	case errors.Is(err, model.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", "conflict")
	case errors.Is(err, model.ErrInvalid):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	default:
		s.Log.Error("request failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

// decode reads a JSON body into v; an empty body leaves v untouched.
func (s *Server) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "body too large")
		return false
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return true
	}
	if err := json.Unmarshal(body, v); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// limit parses ?limit= (default 50, max 200).
func limit(r *http.Request) (int, error) {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return 50, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, newErr(http.StatusBadRequest, "bad_request", "limit must be a positive integer")
	}
	return min(n, 200), nil
}

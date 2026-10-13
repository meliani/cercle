// Package subsonic serves a visibility-filtered Subsonic getNowPlaying
// (docs/CONTRACT.md §5b) so third-party Subsonic apps don't leak what
// everyone is playing.
package subsonic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/meliani/cercle/hub/internal/auth"
	"github.com/meliani/cercle/hub/internal/live"
	"github.com/meliani/cercle/hub/internal/users"
)

// CacheTTL is how long a successful credential check is remembered.
const CacheTTL = 10 * time.Minute

// Handler serves getNowPlaying.
type Handler struct {
	Nav            auth.Navidrome
	Users          *users.Service
	Live           *live.Service
	Limiter        *auth.FailLimiter
	TrustedProxies []netip.Prefix
	Version        string
	Now            func() time.Time
	Log            *slog.Logger

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	user string
	exp  time.Time
}

// Entry is one nowPlaying entry (Subsonic Child + nowPlaying attributes).
type Entry struct {
	ID         string `json:"id" xml:"id,attr"`
	Parent     string `json:"parent,omitempty" xml:"parent,attr,omitempty"`
	IsDir      bool   `json:"isDir" xml:"isDir,attr"`
	Title      string `json:"title" xml:"title,attr"`
	Album      string `json:"album" xml:"album,attr"`
	Artist     string `json:"artist" xml:"artist,attr"`
	AlbumID    string `json:"albumId,omitempty" xml:"albumId,attr,omitempty"`
	ArtistID   string `json:"artistId,omitempty" xml:"artistId,attr,omitempty"`
	Duration   int64  `json:"duration" xml:"duration,attr"`
	CoverArt   string `json:"coverArt" xml:"coverArt,attr"`
	Type       string `json:"type" xml:"type,attr"`
	Username   string `json:"username" xml:"username,attr"`
	MinutesAgo int64  `json:"minutesAgo" xml:"minutesAgo,attr"`
	PlayerID   int    `json:"playerId" xml:"playerId,attr"`
	PlayerName string `json:"playerName" xml:"playerName,attr"`
	State      string `json:"state" xml:"state,attr"`
	PositionMs int64  `json:"positionMs" xml:"positionMs,attr"`
}

type subError struct {
	Code    int    `json:"code" xml:"code,attr"`
	Message string `json:"message" xml:"message,attr"`
}

type nowPlaying struct {
	Entry []Entry `json:"entry" xml:"entry"`
}

// Response is the envelope (attributes in XML, fields in JSON).
type Response struct {
	XMLName       xml.Name    `json:"-" xml:"subsonic-response"`
	Xmlns         string      `json:"-" xml:"xmlns,attr"`
	Status        string      `json:"status" xml:"status,attr"`
	Version       string      `json:"version" xml:"version,attr"`
	Type          string      `json:"type" xml:"type,attr"`
	ServerVersion string      `json:"serverVersion" xml:"serverVersion,attr"`
	OpenSubsonic  bool        `json:"openSubsonic" xml:"openSubsonic,attr"`
	Error         *subError   `json:"error,omitempty" xml:"error,omitempty"`
	NowPlaying    *nowPlaying `json:"nowPlaying,omitempty" xml:"nowPlaying,omitempty"`
}

func (h *Handler) envelope() Response {
	return Response{Xmlns: "http://subsonic.org/restapi", Status: "ok", Version: "1.16.1", Type: "cercle",
		ServerVersion: h.Version, OpenSubsonic: true}
}

func write(w http.ResponseWriter, format string, resp Response) {
	if format == "json" || format == "jsonp" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]Response{"subsonic-response": resp})
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(resp)
}

func (h *Handler) fail(w http.ResponseWriter, format string, code int, msg string) {
	resp := h.envelope()
	resp.Status = "failed"
	resp.Error = &subError{Code: code, Message: msg}
	write(w, format, resp)
}

func cacheKey(c auth.Creds) string {
	sum := sha256.Sum256([]byte(c.Username + "|" + c.Token + "|" + c.Salt + "|" + c.Password + "|" + c.APIKey))
	return hex.EncodeToString(sum[:])
}

func (h *Handler) validate(ctx context.Context, c auth.Creds) (string, error) {
	key := cacheKey(c)
	now := h.Now()
	h.mu.Lock()
	if h.cache == nil {
		h.cache = map[string]cached{}
	}
	if e, ok := h.cache[key]; ok && now.Before(e.exp) {
		h.mu.Unlock()
		return e.user, nil
	}
	h.mu.Unlock()
	id, err := h.Nav.Validate(ctx, c)
	if err != nil {
		return "", err
	}
	// Excluded service accounts are not created; they share no circle, so
	// they simply see an empty nowPlaying.
	if !h.Users.IsExcluded(id.Username) {
		if err := h.Users.EnsureTx(ctx, id.Username, users.EnsureOpts{IsAdmin: &id.IsAdmin}); err != nil {
			return "", err
		}
	}
	h.mu.Lock()
	if len(h.cache) > 10000 {
		for k, e := range h.cache {
			if now.After(e.exp) {
				delete(h.cache, k)
			}
		}
	}
	h.cache[key] = cached{user: id.Username, exp: now.Add(CacheTTL)}
	h.mu.Unlock()
	return id.Username, nil
}

// ServeHTTP handles GET|POST …/rest/getNowPlaying[.view].
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f := strings.ToLower(r.Form.Get("f"))
	c := auth.Creds{Username: r.Form.Get("u"), Token: r.Form.Get("t"), Salt: r.Form.Get("s"),
		Password: r.Form.Get("p"), APIKey: r.Form.Get("apiKey")}
	if !c.Valid() {
		h.fail(w, f, 10, "Required parameter is missing")
		return
	}
	ip := auth.ClientIP(r, h.TrustedProxies)
	if h.Limiter != nil && h.Limiter.Blocked(ip) {
		h.fail(w, f, 40, "Wrong username or password")
		return
	}
	viewer, err := h.validate(r.Context(), c)
	if err != nil {
		if errors.Is(err, auth.ErrBadCredentials) {
			if h.Limiter != nil {
				h.Limiter.Fail(ip)
			}
			h.fail(w, f, 40, "Wrong username or password")
			return
		}
		h.Log.Warn("getNowPlaying validation", "err", err)
		h.fail(w, f, 0, "Navidrome is unavailable")
		return
	}
	visible, err := h.Live.VisibleTo(r.Context(), viewer)
	if err != nil {
		h.Log.Error("getNowPlaying", "err", err)
		h.fail(w, f, 0, "internal error")
		return
	}
	now := h.Now()
	np := &nowPlaying{Entry: []Entry{}}
	for i, l := range visible {
		var updated time.Time
		if t, err := time.Parse(time.RFC3339Nano, l.UpdatedAt); err == nil {
			updated = t
		}
		ago := int64(now.Sub(updated) / time.Minute)
		if ago < 0 {
			ago = 0
		}
		np.Entry = append(np.Entry, Entry{
			ID: l.Track.ID, Parent: l.Track.AlbumID, Title: l.Track.Title, Album: l.Track.Album, Artist: l.Track.Artist,
			AlbumID: l.Track.AlbumID, ArtistID: l.Track.ArtistID, Duration: l.Track.DurationMs / 1000,
			CoverArt: l.Track.ID, Type: "music", Username: l.User.Username, MinutesAgo: ago, PlayerID: i + 1,
			PlayerName: l.PlayerName, State: l.State, PositionMs: l.PositionMs,
		})
	}
	resp := h.envelope()
	resp.NowPlaying = np
	write(w, f, resp)
}

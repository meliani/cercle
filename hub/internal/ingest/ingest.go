// Package ingest implements POST /ingest/v1/events (docs/CONTRACT.md §2):
// plays, feed grouping, discoveries and live state.
package ingest

import (
	"bytes"
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/meliani/cercle/hub/internal/auth"
	"github.com/meliani/cercle/hub/internal/db"
	"github.com/meliani/cercle/hub/internal/feed"
	"github.com/meliani/cercle/hub/internal/live"
	"github.com/meliani/cercle/hub/internal/model"
	"github.com/meliani/cercle/hub/internal/users"
	"github.com/meliani/cercle/hub/internal/visibility"
)

const (
	MaxBatch   = 100
	DedupeMs   = 5000
	GroupGapMs = 30 * 60 * 1000
	maxBody    = 2 << 20
)

// Event is one ingest event. Unknown fields are ignored.
type Event struct {
	Type string `json:"type"`
	User struct {
		Username string `json:"username"`
		Name     string `json:"name"`
		ID       string `json:"id"`
	} `json:"user"`
	Track      TrackIn `json:"track"`
	Player     *Player `json:"player"`
	Timestamp  string  `json:"timestamp"`
	PositionMs *int64  `json:"position_ms"`
	State      string  `json:"state"`
}

type TrackIn struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Artist      string `json:"artist"`
	ArtistID    string `json:"artist_id"`
	Album       string `json:"album"`
	AlbumID     string `json:"album_id"`
	AlbumArtist string `json:"album_artist"`
	DurationMs  int64  `json:"duration_ms"`
	TrackNumber int    `json:"track_number"`
	Year        int    `json:"year"`
	Genre       string `json:"genre"`
	MBZTrackID  string `json:"mbz_track_id"`
	MBZAlbumID  string `json:"mbz_album_id"`
	MBZArtistID string `json:"mbz_artist_id"`
}

type Player struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (t TrackIn) model() model.Track {
	return model.Track{ID: t.ID, Title: t.Title, Artist: t.Artist, ArtistID: t.ArtistID,
		Album: t.Album, AlbumID: t.AlbumID, DurationMs: t.DurationMs}
}

// Service processes events.
type Service struct {
	Secret []byte
	DB     *db.DB
	Users  *users.Service
	Live   *live.Service
	Feed   *feed.Service
	Now    func() time.Time
	Log    *slog.Logger
}

// ServeHTTP implements the ingest endpoint.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tok := auth.BearerToken(r)
	if subtle.ConstantTimeCompare([]byte(tok), s.Secret) != 1 {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "invalid ingest secret")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "too_large", "body too large")
		return
	}
	evs, err := Decode(body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	n := 0
	for i := range evs {
		ok, err := s.Process(r.Context(), &evs[i])
		if err != nil {
			s.Log.Error("ingest event", "type", evs[i].Type, "user", evs[i].User.Username, "err", err)
			continue
		}
		if ok {
			n++
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]int{"accepted": n})
}

// Decode accepts one event object or {"events":[…]} (≤ MaxBatch).
func Decode(body []byte) ([]Event, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 || body[0] != '{' {
		return nil, errors.New("expected a JSON object")
	}
	var probe struct {
		Events json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, errors.New("invalid JSON: " + err.Error())
	}
	if len(probe.Events) > 0 && string(probe.Events) != "null" {
		var evs []Event
		if err := json.Unmarshal(probe.Events, &evs); err != nil {
			return nil, errors.New("invalid events: " + err.Error())
		}
		if len(evs) > MaxBatch {
			return nil, errors.New("too many events (max 100)")
		}
		return evs, nil
	}
	var ev Event
	if err := json.Unmarshal(body, &ev); err != nil {
		return nil, errors.New("invalid event: " + err.Error())
	}
	return []Event{ev}, nil
}

func (s *Service) eventTime(ev *Event) int64 {
	if ev.Timestamp != "" {
		if ms, err := model.ParseTS(ev.Timestamp); err == nil {
			return ms
		}
	}
	return s.Now().UnixMilli()
}

// Process handles one event. It returns false (no error) for events that
// are malformed and skipped; unknown types are accepted and ignored.
func (s *Service) Process(ctx context.Context, ev *Event) (bool, error) {
	ev.User.Username = strings.TrimSpace(ev.User.Username)
	if ev.User.Username == "" {
		return false, nil
	}
	player := Player{}
	if ev.Player != nil {
		player = *ev.Player
	}
	switch ev.Type {
	case "scrobble":
		if ev.Track.ID == "" {
			return false, nil
		}
		return true, s.scrobble(ctx, ev, player)
	case "now_playing":
		if ev.Track.ID == "" {
			return false, nil
		}
		if err := s.ensure(ctx, ev); err != nil {
			return false, err
		}
		pos := int64(0)
		if ev.PositionMs != nil {
			pos = *ev.PositionMs
		}
		s.Live.NowPlaying(ctx, ev.User.Username, ev.Track.model(), s.eventTime(ev), pos, player.ID, player.Name)
		return true, nil
	case "playback_state":
		state := NormalizeState(ev.State)
		if state == "" {
			return false, nil
		}
		if err := s.ensure(ctx, ev); err != nil {
			return false, err
		}
		var t *model.Track
		if ev.Track.ID != "" {
			mt := ev.Track.model()
			t = &mt
		}
		s.Live.PlaybackState(ctx, ev.User.Username, state, t, ev.PositionMs, s.eventTime(ev), player.ID, player.Name)
		return true, nil
	default:
		return true, nil // counted, ignored
	}
}

// NormalizeState maps plugin states to playing|paused|stopped ("" = invalid).
func NormalizeState(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "playing", "starting":
		return "playing"
	case "paused":
		return "paused"
	case "stopped", "expired":
		return "stopped"
	}
	return ""
}

func (s *Service) ensure(ctx context.Context, ev *Event) error {
	return s.DB.Tx(ctx, func(tx *sql.Tx) error {
		_, err := s.Users.Ensure(ctx, tx, ev.User.Username, users.EnsureOpts{Name: ev.User.Name, NavidromeID: ev.User.ID})
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE users SET last_seen_at = ? WHERE username = ?`, s.Now().UnixMilli(), ev.User.Username)
		return err
	})
}

func albumKey(t TrackIn) string {
	if t.AlbumID != "" {
		return "id:" + t.AlbumID
	}
	if t.Album != "" {
		return "name:" + strings.ToLower(t.Album) + "\x00" + strings.ToLower(t.AlbumArtist)
	}
	return ""
}

func (s *Service) scrobble(ctx context.Context, ev *Event, player Player) error {
	user := ev.User.Username
	playedAt := s.eventTime(ev)
	now := s.Now().UnixMilli()
	t := ev.Track
	var changed []string
	err := s.DB.Tx(ctx, func(tx *sql.Tx) error {
		changed = changed[:0]
		if _, err := s.Users.Ensure(ctx, tx, user, users.EnsureOpts{Name: ev.User.Name, NavidromeID: ev.User.ID}); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET last_seen_at = ? WHERE username = ?`, now, user); err != nil {
			return err
		}
		// Dedupe: same user + track within ±5 s.
		var dup int
		err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM plays WHERE user_id = ? AND track_id = ? AND played_at BETWEEN ? AND ?`,
			user, t.ID, playedAt-DedupeMs, playedAt+DedupeMs).Scan(&dup)
		if err != nil {
			return err
		}
		if dup > 0 {
			return nil
		}
		// Private if paused now or matching a hidden item.
		var paused sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT paused_until FROM users WHERE username = ?`, user).Scan(&paused); err != nil {
			return err
		}
		private := paused.Valid && paused.Int64 > now
		if !private {
			hidden, err := visibility.HiddenMatch(ctx, tx, user, t.ID, t.AlbumID, t.ArtistID)
			if err != nil {
				return err
			}
			private = hidden
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO plays (user_id, track_id, title, artist, artist_id, album, album_id,
			album_artist, duration_ms, track_number, year, genre, mbz_track_id, mbz_album_id, mbz_artist_id,
			player_id, player_name, played_at, private, created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			user, t.ID, t.Title, t.Artist, t.ArtistID, t.Album, t.AlbumID, t.AlbumArtist, max(t.DurationMs, 0),
			t.TrackNumber, t.Year, t.Genre, t.MBZTrackID, t.MBZAlbumID, t.MBZArtistID,
			player.ID, player.Name, playedAt, db.Bool(private), now)
		if err != nil {
			return err
		}
		playID, err := res.LastInsertId()
		if err != nil {
			return err
		}
		itemID, err := s.group(ctx, tx, user, playID, playedAt, max(t.DurationMs, 0), albumKey(t), private, now)
		if err != nil {
			return err
		}
		changed = append(changed, itemID)
		disc, err := s.discovery(ctx, tx, user, playID, playedAt, max(t.DurationMs, 0), t, private, now)
		if err != nil {
			return err
		}
		if disc != "" {
			changed = append(changed, disc)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, id := range changed {
		if err := s.Feed.PublishItem(ctx, id); err != nil {
			s.Log.Warn("publish feed item", "item", id, "err", err)
		}
	}
	return nil
}

// group attaches the play to the user's latest `listened` item when it is the
// same album, same privacy, and the gap since that item ended is < 30 min;
// otherwise it starts a new item whose id is the play id.
func (s *Service) group(ctx context.Context, tx *sql.Tx, user string, playID, playedAt, durMs int64, key string, private bool, now int64) (string, error) {
	var id, lastKey string
	var ended, started int64
	var lastPrivate int
	err := tx.QueryRowContext(ctx, `SELECT id, album_key, started_at, ended_at, private FROM feed_items
		WHERE user_id = ? AND kind = 'listened' ORDER BY ended_at DESC, id DESC LIMIT 1`, user).
		Scan(&id, &lastKey, &started, &ended, &lastPrivate)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	end := playedAt + durMs
	if err == nil && key != "" && key == lastKey && (lastPrivate == 1) == private &&
		playedAt >= started && playedAt-ended < GroupGapMs {
		_, err := tx.ExecContext(ctx, `UPDATE feed_items SET
			last_play_id = CASE WHEN ? >= last_played_at THEN ? ELSE last_play_id END,
			play_count = play_count + 1,
			ended_at = MAX(ended_at, ?), last_played_at = MAX(last_played_at, ?), updated_at = ?
			WHERE id = ?`, playedAt, playID, end, playedAt, now, id)
		if err != nil {
			return "", err
		}
		_, err = tx.ExecContext(ctx, `UPDATE plays SET feed_item_id = ? WHERE id = ?`, id, playID)
		return id, err
	}
	id = strconv.FormatInt(playID, 10)
	if _, err := tx.ExecContext(ctx, `INSERT INTO feed_items (id, kind, user_id, first_play_id, last_play_id, album_key,
		play_count, started_at, ended_at, last_played_at, private, updated_at)
		VALUES (?, 'listened', ?, ?, ?, ?, 1, ?, ?, ?, ?, ?)`,
		id, user, playID, playID, key, playedAt, end, playedAt, db.Bool(private), now); err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `UPDATE plays SET feed_item_id = ? WHERE id = ?`, id, playID)
	return id, err
}

// discovery creates a discovery_artist item on the first ever play of an
// artist, else discovery_album on the first play of an album. Empty ids
// never produce discoveries.
func (s *Service) discovery(ctx context.Context, tx *sql.Tx, user string, playID, playedAt, durMs int64, t TrackIn, private bool, now int64) (string, error) {
	kind := ""
	if t.ArtistID != "" {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM plays WHERE user_id = ? AND artist_id = ? AND id <> ?`,
			user, t.ArtistID, playID).Scan(&n); err != nil {
			return "", err
		}
		if n == 0 {
			kind = "discovery_artist"
		}
	}
	if kind == "" && t.AlbumID != "" {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM plays WHERE user_id = ? AND album_id = ? AND id <> ?`,
			user, t.AlbumID, playID).Scan(&n); err != nil {
			return "", err
		}
		if n == 0 {
			kind = "discovery_album"
		}
	}
	if kind == "" {
		return "", nil
	}
	suffix := "-artist"
	if kind == "discovery_album" {
		suffix = "-album"
	}
	id := strconv.FormatInt(playID, 10) + suffix
	_, err := tx.ExecContext(ctx, `INSERT INTO feed_items (id, kind, user_id, first_play_id, last_play_id, album_key,
		play_count, started_at, ended_at, last_played_at, private, updated_at)
		VALUES (?, ?, ?, ?, ?, '', 1, ?, ?, ?, ?, ?)`,
		id, kind, user, playID, playID, playedAt, playedAt+durMs, playedAt, db.Bool(private), now)
	return id, err
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

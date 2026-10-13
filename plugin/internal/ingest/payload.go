// Package ingest turns Navidrome scrobbler events into Cercle hub ingest
// payloads (docs/CONTRACT.md §2). It is pure Go with no Navidrome PDK imports,
// so it builds and tests natively; the wasm entry point in the parent package
// only copies PDK request structs into the types below.
package ingest

import (
	"math"
	"strings"
	"time"
)

// Event types understood by the hub.
const (
	TypeNowPlaying    = "now_playing"
	TypeScrobble      = "scrobble"
	TypePlaybackState = "playback_state"
)

// IngestPath is appended to the configured hub URL.
const IngestPath = "/ingest/v1/events"

// ArtistRef mirrors the fields of the PDK's types.ArtistRef that we use.
type ArtistRef struct {
	ID   string
	Name string
	MBID string
}

// Track mirrors the fields of the PDK's scrobbler.TrackInfo that we use.
// Duration is in seconds, as Navidrome sends it.
type Track struct {
	ID             string
	Title          string
	Album          string
	Artist         string
	AlbumArtist    string
	Artists        []ArtistRef
	Duration       float32
	TrackNumber    int32
	MBZRecordingID string
	MBZAlbumID     string
}

// Event is one hub ingest event.
type Event struct {
	Type       string       `json:"type"`
	User       User         `json:"user"`
	Track      TrackPayload `json:"track"`
	Player     *Player      `json:"player,omitempty"`
	Timestamp  string       `json:"timestamp"`
	PositionMs *int64       `json:"position_ms,omitempty"`
	State      string       `json:"state,omitempty"`
}

// User identifies the listener. Navidrome only gives plugins the username;
// Name is filled from the users host service when available. ID is never
// known to the plugin and is always omitted.
type User struct {
	Username string `json:"username"`
	Name     string `json:"name,omitempty"`
	ID       string `json:"id,omitempty"`
}

// TrackPayload is the contract's track object.
type TrackPayload struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Artist      string `json:"artist"`
	ArtistID    string `json:"artist_id,omitempty"`
	Album       string `json:"album"`
	AlbumID     string `json:"album_id,omitempty"`
	AlbumArtist string `json:"album_artist,omitempty"`
	DurationMs  int64  `json:"duration_ms"`
	TrackNumber int32  `json:"track_number,omitempty"`
	Year        int    `json:"year,omitempty"`
	Genre       string `json:"genre,omitempty"`
	MBZTrackID  string `json:"mbz_track_id,omitempty"`
	MBZAlbumID  string `json:"mbz_album_id,omitempty"`
	MBZArtistID string `json:"mbz_artist_id,omitempty"`
}

// Player is the client that produced a playback report.
type Player struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// NowPlaying maps a NowPlaying call. position is in seconds.
func NowPlaying(username string, t Track, position int32, now time.Time) Event {
	pos := int64(position) * 1000
	return Event{
		Type:       TypeNowPlaying,
		User:       User{Username: username},
		Track:      mapTrack(t),
		Timestamp:  formatTime(now),
		PositionMs: &pos,
	}
}

// Scrobble maps a Scrobble call. ts is the unix time (seconds) Navidrome
// reports; 0 falls back to now.
func Scrobble(username string, t Track, ts int64, now time.Time) Event {
	return Event{
		Type:      TypeScrobble,
		User:      User{Username: username},
		Track:     mapTrack(t),
		Timestamp: unixOrNow(ts, now),
	}
}

// PlaybackState maps a PlaybackReport call. The state is passed through
// unchanged (starting|playing|paused|stopped|expired); the hub normalises it.
func PlaybackState(username string, t Track, state string, positionMs int64, playerID, playerName string, ts int64, now time.Time) Event {
	pos := positionMs
	if pos < 0 {
		pos = 0
	}
	ev := Event{
		Type:       TypePlaybackState,
		User:       User{Username: username},
		Track:      mapTrack(t),
		Timestamp:  unixOrNow(ts, now),
		PositionMs: &pos,
		State:      state,
	}
	if playerID != "" || playerName != "" {
		ev.Player = &Player{ID: playerID, Name: playerName}
	}
	return ev
}

func mapTrack(t Track) TrackPayload {
	p := TrackPayload{
		ID:          t.ID,
		Title:       t.Title,
		Artist:      t.Artist,
		Album:       t.Album,
		AlbumArtist: t.AlbumArtist,
		DurationMs:  secondsToMs(t.Duration),
		TrackNumber: t.TrackNumber,
		MBZTrackID:  t.MBZRecordingID,
		MBZAlbumID:  t.MBZAlbumID,
	}
	if len(t.Artists) > 0 {
		p.ArtistID = t.Artists[0].ID
		p.MBZArtistID = t.Artists[0].MBID
		if p.Artist == "" {
			p.Artist = t.Artists[0].Name
		}
	}
	return p
}

func secondsToMs(s float32) int64 {
	if s <= 0 || math.IsNaN(float64(s)) || math.IsInf(float64(s), 0) {
		return 0
	}
	return int64(math.Round(float64(s) * 1000))
}

func unixOrNow(ts int64, now time.Time) string {
	if ts <= 0 {
		return formatTime(now)
	}
	return formatTime(time.Unix(ts, 0))
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// EndpointURL builds the ingest URL from the configured hub base URL.
func EndpointURL(hubURL string) string {
	return strings.TrimRight(strings.TrimSpace(hubURL), "/") + IngestPath
}

// ValidHubURL reports whether the configured hub URL is usable.
func ValidHubURL(hubURL string) bool {
	u := strings.ToLower(strings.TrimSpace(hubURL))
	return (strings.HasPrefix(u, "http://") && len(u) > len("http://")) ||
		(strings.HasPrefix(u, "https://") && len(u) > len("https://"))
}

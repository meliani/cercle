// Package model holds the JSON wire types of docs/CONTRACT.md §4.
package model

import (
	"errors"
	"time"
)

// Common errors shared by services; the API maps them to HTTP statuses.
var (
	ErrNotFound  = errors.New("not found")
	ErrForbidden = errors.New("forbidden")
	ErrConflict  = errors.New("conflict")
	ErrInvalid   = errors.New("invalid")
)

// TimeLayout is RFC 3339 in UTC with millisecond precision (room sync needs ms).
const TimeLayout = "2006-01-02T15:04:05.000Z"

// TS formats unix milliseconds.
func TS(ms int64) string { return time.UnixMilli(ms).UTC().Format(TimeLayout) }

// TSPtr formats a nullable unix-ms value.
func TSPtr(ms *int64) *string {
	if ms == nil {
		return nil
	}
	s := TS(*ms)
	return &s
}

// ParseTS parses an RFC 3339 timestamp into unix ms.
func ParseTS(s string) (int64, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0, err
	}
	return t.UnixMilli(), nil
}

type User struct {
	Username    string  `json:"username"`
	DisplayName string  `json:"display_name"`
	IsAdmin     bool    `json:"is_admin"`
	CreatedAt   string  `json:"created_at"`
	LastSeenAt  *string `json:"last_seen_at"`
}

type Me struct {
	User
	Settings Settings `json:"settings"`
}

type Settings struct {
	PausedUntil         *string  `json:"paused_until"`
	HistoryDelayMinutes int      `json:"history_delay_minutes"`
	Hidden              []Hidden `json:"hidden"`
}

type Hidden struct {
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	Label string `json:"label"`
}

type Circle struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Kind        string  `json:"kind"`
	MemberCount int     `json:"member_count"`
	MyRole      *string `json:"my_role"`
	CreatedAt   string  `json:"created_at"`
}

type CircleDetail struct {
	Circle
	Members []Member `json:"members"`
}

type Member struct {
	User     User     `json:"user"`
	Role     string   `json:"role"`
	JoinedAt string   `json:"joined_at"`
	Sharing  *Sharing `json:"sharing,omitempty"`
}

type Sharing struct {
	ShareLive        bool `json:"share_live"`
	ShareHistory     bool `json:"share_history"`
	ShareStats       bool `json:"share_stats"`
	ShareDiscoveries bool `json:"share_discoveries"`
}

type Invite struct {
	Code      string `json:"code"`
	CircleID  string `json:"circle_id"`
	CreatedBy string `json:"created_by"`
	ExpiresAt string `json:"expires_at"`
	MaxUses   int    `json:"max_uses"`
	Uses      int    `json:"uses"`
	URLPath   string `json:"url_path"`
}

type Track struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	ArtistID   string `json:"artist_id"`
	Album      string `json:"album"`
	AlbumID    string `json:"album_id"`
	DurationMs int64  `json:"duration_ms"`
}

type Live struct {
	User       User   `json:"user"`
	Track      Track  `json:"track"`
	State      string `json:"state"`
	PositionMs int64  `json:"position_ms"`
	StartedAt  string `json:"started_at"`
	UpdatedAt  string `json:"updated_at"`
	PlayerName string `json:"player_name"`
}

type AlbumRef struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Artist string `json:"artist"`
}

type ArtistRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Reaction struct {
	Emoji string `json:"emoji"`
	Count int    `json:"count"`
	Mine  bool   `json:"mine"`
}

type FeedItem struct {
	ID           string     `json:"id"`
	Kind         string     `json:"kind"`
	User         User       `json:"user"`
	Track        *Track     `json:"track,omitempty"`
	Album        *AlbumRef  `json:"album,omitempty"`
	Artist       *ArtistRef `json:"artist,omitempty"`
	PlayCount    int        `json:"play_count"`
	StartedAt    string     `json:"started_at"`
	EndedAt      string     `json:"ended_at"`
	Reactions    []Reaction `json:"reactions"`
	CommentCount int        `json:"comment_count"`
}

type Comment struct {
	ID        string  `json:"id"`
	User      User    `json:"user"`
	Body      string  `json:"body"`
	CreatedAt string  `json:"created_at"`
	EditedAt  *string `json:"edited_at"`
}

type Play struct {
	ID         string `json:"id"`
	Track      Track  `json:"track"`
	PlayedAt   string `json:"played_at"`
	PlayerName string `json:"player_name"`
}

type StatEntry struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Artist  string `json:"artist,omitempty"`
	AlbumID string `json:"album_id,omitempty"`
	Plays   int64  `json:"plays"`
	Ms      int64  `json:"ms"`
}

type Totals struct {
	Plays           int64 `json:"plays"`
	Ms              int64 `json:"ms"`
	ActiveDays      int   `json:"active_days"`
	DistinctArtists int64 `json:"distinct_artists"`
	DistinctAlbums  int64 `json:"distinct_albums"`
}

type DayCount struct {
	Date  string `json:"date"`
	Plays int64  `json:"plays"`
}

type Stats struct {
	Range      string      `json:"range"`
	Totals     Totals      `json:"totals"`
	TopArtists []StatEntry `json:"top_artists"`
	TopAlbums  []StatEntry `json:"top_albums"`
	TopTracks  []StatEntry `json:"top_tracks"`
	ByHour     [24]int64   `json:"by_hour"`
	ByWeekday  [7]int64    `json:"by_weekday"`
	ByDay      []DayCount  `json:"by_day"`
}

type MemberStat struct {
	User  User  `json:"user"`
	Plays int64 `json:"plays"`
	Ms    int64 `json:"ms"`
}

type CircleStats struct {
	Stats
	Members []MemberStat `json:"members"`
}

type Compare struct {
	CommonArtists []StatEntry `json:"common_artists"`
	CommonAlbums  []StatEntry `json:"common_albums"`
	Score         float64     `json:"score"`
}

type FirstPlay struct {
	Track    Track  `json:"track"`
	PlayedAt string `json:"played_at"`
}

type DayMs struct {
	Date string `json:"date"`
	Ms   int64  `json:"ms"`
}

type Wrapped struct {
	Stats
	FirstPlay  *FirstPlay `json:"first_play"`
	TopMonth   *string    `json:"top_month"`
	LongestDay *DayMs     `json:"longest_day"`
	NewArtists int        `json:"new_artists"`
}

type RoomState struct {
	TrackID    *string  `json:"track_id"`
	PositionMs int64    `json:"position_ms"`
	Playing    bool     `json:"playing"`
	Queue      []string `json:"queue"`
	UpdatedAt  string   `json:"updated_at"`
	Seq        int64    `json:"seq"`
}

type Room struct {
	ID        string    `json:"id"`
	CircleID  string    `json:"circle_id"`
	Name      string    `json:"name"`
	Host      User      `json:"host"`
	Listeners []User    `json:"listeners"`
	State     RoomState `json:"state"`
	CreatedAt string    `json:"created_at"`
}

type RoomRequest struct {
	ID        string `json:"id"`
	User      User   `json:"user"`
	Track     Track  `json:"track"`
	CreatedAt string `json:"created_at"`
}

type RoomDetail struct {
	Room
	Requests []RoomRequest `json:"requests"`
}

// Page is the paginated list envelope.
type Page[T any] struct {
	Items []T     `json:"items"`
	Next  *string `json:"next"`
}

// List is the non-paginated list envelope.
type List[T any] struct {
	Items []T `json:"items"`
}

package ingest

import (
	"encoding/json"
	"errors"
	"regexp"
	"testing"
	"time"
)

var now = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func sampleTrack() Track {
	return Track{
		ID:          "mf-1",
		Title:       "Time",
		Album:       "The Dark Side of the Moon",
		Artist:      "Pink Floyd",
		AlbumArtist: "Pink Floyd",
		Artists: []ArtistRef{
			{ID: "ar-1", Name: "Pink Floyd", MBID: "mbz-artist"},
			{ID: "ar-2", Name: "Guest"},
		},
		Duration:       245.0,
		TrackNumber:    4,
		MBZRecordingID: "mbz-rec",
		MBZAlbumID:     "mbz-album",
	}
}

// assertJSON compares the marshalled event with the expected contract JSON,
// ignoring formatting and key order.
func assertJSON(t *testing.T, got any, want string) {
	t.Helper()
	gb, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var g, w any
	if err := json.Unmarshal(gb, &g); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("bad expected JSON: %v", err)
	}
	gn, _ := json.Marshal(g)
	wn, _ := json.Marshal(w)
	if string(gn) != string(wn) {
		t.Errorf("JSON mismatch\n got: %s\nwant: %s", gn, wn)
	}
}

func TestNowPlayingMapping(t *testing.T) {
	ev := NowPlaying("alice", sampleTrack(), 12, now)
	assertJSON(t, ev, `{
		"type": "now_playing",
		"user": {"username": "alice"},
		"track": {
			"id": "mf-1", "title": "Time", "artist": "Pink Floyd", "artist_id": "ar-1",
			"album": "The Dark Side of the Moon", "album_artist": "Pink Floyd",
			"duration_ms": 245000, "track_number": 4,
			"mbz_track_id": "mbz-rec", "mbz_album_id": "mbz-album", "mbz_artist_id": "mbz-artist"
		},
		"timestamp": "2026-01-01T12:00:00Z",
		"position_ms": 12000
	}`)
}

func TestNowPlayingAtZeroKeepsPosition(t *testing.T) {
	ev := NowPlaying("alice", sampleTrack(), 0, now)
	b, _ := json.Marshal(ev)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if v, ok := m["position_ms"]; !ok || v.(float64) != 0 {
		t.Errorf("position_ms should be present and 0, got %v (present=%v)", v, ok)
	}
}

func TestScrobbleMapping(t *testing.T) {
	ts := time.Date(2026, 1, 1, 11, 55, 30, 0, time.UTC).Unix()
	ev := Scrobble("bob", sampleTrack(), ts, now)
	assertJSON(t, ev, `{
		"type": "scrobble",
		"user": {"username": "bob"},
		"track": {
			"id": "mf-1", "title": "Time", "artist": "Pink Floyd", "artist_id": "ar-1",
			"album": "The Dark Side of the Moon", "album_artist": "Pink Floyd",
			"duration_ms": 245000, "track_number": 4,
			"mbz_track_id": "mbz-rec", "mbz_album_id": "mbz-album", "mbz_artist_id": "mbz-artist"
		},
		"timestamp": "2026-01-01T11:55:30Z"
	}`)
}

func TestScrobbleZeroTimestampFallsBackToNow(t *testing.T) {
	ev := Scrobble("bob", sampleTrack(), 0, now)
	if ev.Timestamp != "2026-01-01T12:00:00Z" {
		t.Errorf("timestamp = %s", ev.Timestamp)
	}
}

func TestTimestampIsUTC(t *testing.T) {
	loc := time.FixedZone("X", 3600)
	ev := NowPlaying("alice", sampleTrack(), 0, time.Date(2026, 1, 1, 13, 0, 0, 0, loc))
	if ev.Timestamp != "2026-01-01T12:00:00Z" {
		t.Errorf("timestamp = %s, want UTC", ev.Timestamp)
	}
}

func TestPlaybackStateMapping(t *testing.T) {
	ts := now.Unix()
	for _, state := range []string{"starting", "playing", "paused", "stopped", "expired"} {
		ev := PlaybackState("carol", sampleTrack(), state, 61500, "player-1", "Feishin", ts, now)
		assertJSON(t, ev, `{
			"type": "playback_state",
			"user": {"username": "carol"},
			"track": {
				"id": "mf-1", "title": "Time", "artist": "Pink Floyd", "artist_id": "ar-1",
				"album": "The Dark Side of the Moon", "album_artist": "Pink Floyd",
				"duration_ms": 245000, "track_number": 4,
				"mbz_track_id": "mbz-rec", "mbz_album_id": "mbz-album", "mbz_artist_id": "mbz-artist"
			},
			"player": {"id": "player-1", "name": "Feishin"},
			"timestamp": "2026-01-01T12:00:00Z",
			"position_ms": 61500,
			"state": "`+state+`"
		}`)
	}
}

func TestPlaybackStateWithoutPlayer(t *testing.T) {
	ev := PlaybackState("carol", sampleTrack(), "paused", -5, "", "", 0, now)
	if ev.Player != nil {
		t.Errorf("player should be omitted, got %+v", ev.Player)
	}
	if *ev.PositionMs != 0 {
		t.Errorf("negative position should clamp to 0, got %d", *ev.PositionMs)
	}
}

func TestDurationRounding(t *testing.T) {
	tr := sampleTrack()
	tr.Duration = 245.4567
	if got := NowPlaying("a", tr, 0, now).Track.DurationMs; got != 245457 {
		t.Errorf("duration_ms = %d, want 245457", got)
	}
	tr.Duration = 0
	if got := NowPlaying("a", tr, 0, now).Track.DurationMs; got != 0 {
		t.Errorf("duration_ms = %d, want 0", got)
	}
}

func TestTrackWithoutArtistsOmitsIDs(t *testing.T) {
	tr := Track{ID: "mf-2", Title: "Untitled", Album: "Demo", Artist: "Someone", Duration: 10}
	assertJSON(t, Scrobble("alice", tr, now.Unix(), now), `{
		"type": "scrobble",
		"user": {"username": "alice"},
		"track": {"id": "mf-2", "title": "Untitled", "artist": "Someone", "album": "Demo", "duration_ms": 10000},
		"timestamp": "2026-01-01T12:00:00Z"
	}`)
}

const getSongOK = `{"subsonic-response":{"status":"ok","version":"1.16.1","type":"navidrome",
  "song":{"id":"mf-1","title":"Time","album":"The Dark Side of the Moon","albumId":"al-9",
  "artist":"Pink Floyd","artistId":"ar-1","year":1973,"genre":"Rock",
  "genres":[{"name":"Rock"},{"name":"Progressive"}],"artists":[{"id":"ar-1","name":"Pink Floyd"}]}}}`

func TestParseGetSong(t *testing.T) {
	en, err := ParseGetSong(getSongOK)
	if err != nil {
		t.Fatal(err)
	}
	want := Enrichment{AlbumID: "al-9", ArtistID: "ar-1", Year: 1973, Genre: "Rock"}
	if en != want {
		t.Errorf("got %+v, want %+v", en, want)
	}
}

func TestParseGetSongFallbacks(t *testing.T) {
	body := `{"subsonic-response":{"status":"ok","song":{"id":"x","albumId":"al","genres":[{"name":"Jazz"}],"artists":[{"id":"ar-7"}]}}}`
	en, err := ParseGetSong(body)
	if err != nil {
		t.Fatal(err)
	}
	if en.ArtistID != "ar-7" || en.Genre != "Jazz" || en.AlbumID != "al" {
		t.Errorf("got %+v", en)
	}
}

func TestParseGetSongErrors(t *testing.T) {
	cases := map[string]string{
		"failed":  `{"subsonic-response":{"status":"failed","error":{"code":70,"message":"Song not found"}}}`,
		"no song": `{"subsonic-response":{"status":"ok"}}`,
		"garbage": `not json`,
	}
	for name, body := range cases {
		if _, err := ParseGetSong(body); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestGetSongURIEscapes(t *testing.T) {
	got := GetSongURI("a&b c", "id/1")
	if got != "getSong?id=id%2F1&u=a%26b+c" {
		t.Errorf("uri = %s", got)
	}
}

func TestApplyEnrichment(t *testing.T) {
	// Track artist id from Navidrome wins over getSong's artistId.
	ev := Scrobble("alice", sampleTrack(), now.Unix(), now)
	ev.Apply(Enrichment{AlbumID: "al-9", ArtistID: "other", Year: 1973, Genre: "Rock"})
	if ev.Track.ArtistID != "ar-1" || ev.Track.AlbumID != "al-9" || ev.Track.Year != 1973 || ev.Track.Genre != "Rock" {
		t.Errorf("merged track = %+v", ev.Track)
	}
	// Without track artists, getSong fills the artist id.
	ev = Scrobble("alice", Track{ID: "mf-3", Title: "T"}, now.Unix(), now)
	ev.Apply(Enrichment{AlbumID: "al-1", ArtistID: "ar-5"})
	if ev.Track.ArtistID != "ar-5" || ev.Track.AlbumID != "al-1" {
		t.Errorf("merged track = %+v", ev.Track)
	}
	// Empty enrichment changes nothing.
	before := Scrobble("alice", sampleTrack(), now.Unix(), now)
	after := before
	after.Apply(Enrichment{})
	if before.Track != after.Track {
		t.Errorf("empty enrichment changed the track")
	}
}

func TestEnrichedPayloadJSON(t *testing.T) {
	ev := Scrobble("alice", sampleTrack(), now.Unix(), now)
	ev.User.Name = "Alice"
	ev.Apply(Enrichment{AlbumID: "al-9", Year: 1973, Genre: "Rock"})
	assertJSON(t, ev, `{
		"type": "scrobble",
		"user": {"username": "alice", "name": "Alice"},
		"track": {
			"id": "mf-1", "title": "Time", "artist": "Pink Floyd", "artist_id": "ar-1",
			"album": "The Dark Side of the Moon", "album_id": "al-9", "album_artist": "Pink Floyd",
			"duration_ms": 245000, "track_number": 4, "year": 1973, "genre": "Rock",
			"mbz_track_id": "mbz-rec", "mbz_album_id": "mbz-album", "mbz_artist_id": "mbz-artist"
		},
		"timestamp": "2026-01-01T12:00:00Z"
	}`)
}

func TestEnrichmentCacheRoundTrip(t *testing.T) {
	en := Enrichment{AlbumID: "al", ArtistID: "ar", Year: 2001, Genre: "Pop"}
	got, err := DecodeEnrichment(EncodeEnrichment(en))
	if err != nil || got != en {
		t.Errorf("round trip = %+v, %v", got, err)
	}
}

// Same regex Navidrome 0.64 uses to recognise retry_later from a plugin.
var ndRetryLater = regexp.MustCompile(`\bscrobbler\(retry_later(?::(\d+))?\)`)

func TestClassify(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		headers map[string]string
		err     error
		want    Outcome
		token   string
	}{
		{"accepted", 202, nil, nil, Delivered, ""},
		{"ok", 200, nil, nil, Delivered, ""},
		{"network error", 0, nil, errors.New("dial tcp: connection refused"), RetryLater, "scrobbler(retry_later)"},
		{"500", 500, nil, nil, RetryLater, "scrobbler(retry_later)"},
		{"502", 502, nil, nil, RetryLater, "scrobbler(retry_later)"},
		{"503 retry-after", 503, map[string]string{"retry-after": "30"}, nil, RetryLater, "scrobbler(retry_later:30)"},
		{"429", 429, map[string]string{"Retry-After": "7"}, nil, RetryLater, "scrobbler(retry_later:7)"},
		{"408", 408, nil, nil, RetryLater, "scrobbler(retry_later)"},
		{"400", 400, nil, nil, Unrecoverable, "scrobbler(unrecoverable)"},
		{"401", 401, nil, nil, Unrecoverable, "scrobbler(unrecoverable)"},
		{"404", 404, nil, nil, Unrecoverable, "scrobbler(unrecoverable)"},
		{"413", 413, nil, nil, Unrecoverable, "scrobbler(unrecoverable)"},
	}
	for _, c := range cases {
		r := Classify(c.status, c.headers, c.err)
		if r.Outcome != c.want {
			t.Errorf("%s: outcome = %v, want %v", c.name, r.Outcome, c.want)
		}
		tok := ScrobbleErrorToken(r)
		if tok != c.token {
			t.Errorf("%s: token = %q, want %q", c.name, tok, c.token)
		}
		if c.want == RetryLater && !ndRetryLater.MatchString(tok+": detail") {
			t.Errorf("%s: token %q not recognised by Navidrome's regex", c.name, tok)
		}
		if c.want != Delivered && r.Detail == "" {
			t.Errorf("%s: missing detail", c.name)
		}
	}
}

func TestDeliverBuildsRequest(t *testing.T) {
	var got Request
	post := func(r Request) (int, map[string]string, error) {
		got = r
		return 202, nil, nil
	}
	ev := NowPlaying("alice", sampleTrack(), 3, now)
	res := Deliver(post, "http://cercle-hub:8080/hub/", "s3cret-s3cret-s3cret-s3cret-s3cret", ev)
	if res.Outcome != Delivered {
		t.Fatalf("outcome = %v", res.Outcome)
	}
	if got.URL != "http://cercle-hub:8080/hub/ingest/v1/events" {
		t.Errorf("url = %s", got.URL)
	}
	if got.Headers["Authorization"] != "Bearer s3cret-s3cret-s3cret-s3cret-s3cret" {
		t.Errorf("auth header = %q", got.Headers["Authorization"])
	}
	if got.Headers["Content-Type"] != "application/json" {
		t.Errorf("content-type = %q", got.Headers["Content-Type"])
	}
	var back Event
	if err := json.Unmarshal(got.Body, &back); err != nil || back.Type != TypeNowPlaying || *back.PositionMs != 3000 {
		t.Errorf("body = %s (%v)", got.Body, err)
	}
}

func TestDeliverClassifiesFailures(t *testing.T) {
	ev := Scrobble("alice", sampleTrack(), now.Unix(), now)
	down := func(Request) (int, map[string]string, error) { return 0, nil, errors.New("timeout") }
	if r := Deliver(down, "http://hub", "x", ev); r.Outcome != RetryLater {
		t.Errorf("network error: %v", r.Outcome)
	}
	bad := func(Request) (int, map[string]string, error) { return 401, nil, nil }
	r := Deliver(bad, "http://hub", "wrong", ev)
	if r.Outcome != Unrecoverable {
		t.Errorf("401: %v", r.Outcome)
	}
	if regexp.MustCompile("wrong").MatchString(r.Detail) {
		t.Errorf("detail leaks the secret: %s", r.Detail)
	}
}

func TestEndpointAndValidation(t *testing.T) {
	if EndpointURL(" https://example.com ") != "https://example.com/ingest/v1/events" {
		t.Error("trim")
	}
	if EndpointURL("https://example.com/hub//") != "https://example.com/hub/ingest/v1/events" {
		t.Error("trailing slashes")
	}
	for _, ok := range []string{"http://cercle-hub:8080", "https://example.com/hub", "HTTPS://example.com"} {
		if !ValidHubURL(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "example.com", "ftp://example.com", "http://", " "} {
		if ValidHubURL(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

//go:build wasip1

// Cercle Navidrome plugin: forwards scrobbler events (now playing, scrobbles,
// playback reports) to a Cercle hub's ingest API. See docs/CONTRACT.md §2/§7.
//
// Build (standard Go >= 1.25):
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o plugin.wasm .
package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/meliani/cercle/plugin/internal/ingest"
	"github.com/navidrome/navidrome/plugins/pdk/go/host"
	"github.com/navidrome/navidrome/plugins/pdk/go/pdk"
	"github.com/navidrome/navidrome/plugins/pdk/go/scrobbler"
)

const (
	cfgHubURL = "hub_url"
	cfgSecret = "ingest_secret"

	httpTimeoutMs = 5000

	songCachePrefix = "song:"
	songCacheTTL    = int64(12 * 60 * 60) // track metadata rarely changes
	userCachePrefix = "user:"
	userCacheTTL    = int64(60 * 60)
	warnCacheKey    = "warned:config"
	warnCacheTTL    = int64(60 * 60)
)

type cercle struct{}

func init() {
	scrobbler.Register(&cercle{})
}

func main() {}

var _ scrobbler.Scrobbler = (*cercle)(nil)

type config struct {
	hubURL string
	secret string
}

func loadConfig() (config, bool) {
	hubURL, _ := pdk.GetConfig(cfgHubURL)
	secret, _ := pdk.GetConfig(cfgSecret)
	c := config{hubURL: strings.TrimSpace(hubURL), secret: strings.TrimSpace(secret)}
	return c, ingest.ValidHubURL(c.hubURL) && c.secret != ""
}

// IsAuthorized runs before every event, so it does no network I/O: a user is
// "authorized" as soon as the plugin has a hub URL and a secret. Which users
// reach the plugin at all is decided by Navidrome's per-plugin user list.
func (c *cercle) IsAuthorized(scrobbler.IsAuthorizedRequest) (bool, error) {
	_, ok := loadConfig()
	if !ok {
		warnMisconfigured()
	}
	return ok, nil
}

// warnMisconfigured logs at most once per hour. Navidrome instantiates the
// module afresh for each call, so the rate limit lives in the host cache.
func warnMisconfigured() {
	if seen, err := host.CacheHas(warnCacheKey); err == nil && seen {
		return
	}
	_ = host.CacheSetString(warnCacheKey, "1", warnCacheTTL)
	pdk.Log(pdk.LogWarn, "cercle: hub_url (http/https) and ingest_secret must be set in the plugin configuration; events are not forwarded")
}

func (c *cercle) NowPlaying(req scrobbler.NowPlayingRequest) error {
	ev := ingest.NowPlaying(req.Username, toTrack(req.Track), req.Position, time.Now())
	sendLive(ev)
	return nil
}

func (c *cercle) PlaybackReport(req scrobbler.PlaybackReportRequest) error {
	ev := ingest.PlaybackState(req.Username, toTrack(req.Track), req.State, req.PositionMs,
		req.PlayerId, req.PlayerName, req.Timestamp, time.Now())
	sendLive(ev)
	return nil
}

// Scrobble is the only event Navidrome buffers and retries, and only when we
// answer retry_later. Network errors and 5xx therefore return retry_later so
// plays survive a hub outage; a 4xx means the hub will never accept it.
func (c *cercle) Scrobble(req scrobbler.ScrobbleRequest) error {
	cfg, ok := loadConfig()
	if !ok {
		return fmt.Errorf("%w: plugin not configured", scrobbler.ScrobblerErrorUnrecoverable)
	}
	ev := ingest.Scrobble(req.Username, toTrack(req.Track), req.Timestamp, time.Now())
	enrich(&ev)
	res := ingest.Deliver(post, cfg.hubURL, cfg.secret, ev)
	switch res.Outcome {
	case ingest.Delivered:
		return nil
	case ingest.RetryLater:
		pdk.Log(pdk.LogWarn, fmt.Sprintf("cercle: scrobble for %q deferred: %s", req.Username, res.Detail))
		if res.RetryAfter > 0 {
			return fmt.Errorf("%s: %s", ingest.ScrobbleErrorToken(res), res.Detail)
		}
		return fmt.Errorf("%w: %s", scrobbler.ScrobblerErrorRetryLater, res.Detail)
	default:
		pdk.Log(pdk.LogError, fmt.Sprintf("cercle: scrobble for %q dropped: %s", req.Username, res.Detail))
		return fmt.Errorf("%w: %s", scrobbler.ScrobblerErrorUnrecoverable, res.Detail)
	}
}

// sendLive delivers a now_playing / playback_state event once. These are
// superseded within seconds, so failures are logged and dropped.
func sendLive(ev ingest.Event) {
	cfg, ok := loadConfig()
	if !ok {
		return
	}
	enrich(&ev)
	res := ingest.Deliver(post, cfg.hubURL, cfg.secret, ev)
	if res.Outcome != ingest.Delivered {
		pdk.Log(pdk.LogWarn, fmt.Sprintf("cercle: %s for %q dropped: %s", ev.Type, ev.User.Username, res.Detail))
	}
}

func post(r ingest.Request) (int, map[string]string, error) {
	resp, err := host.HTTPSend(host.HTTPRequest{
		Method:    "POST",
		URL:       r.URL,
		Headers:   r.Headers,
		Body:      r.Body,
		TimeoutMs: httpTimeoutMs,
	})
	if err != nil {
		return 0, nil, err
	}
	if resp == nil {
		return 0, nil, fmt.Errorf("empty response")
	}
	return int(resp.StatusCode), resp.Headers, nil
}

// enrich fills album id, artist id, year and genre (via the host Subsonic
// API, cached per track) and the user's display name. Any failure leaves the
// fields empty; the hub accepts events without them.
func enrich(ev *ingest.Event) {
	ev.User.Name = displayName(ev.User.Username)
	if ev.Track.ID == "" || ev.User.Username == "" {
		return
	}
	en, err := songEnrichment(ev.User.Username, ev.Track.ID)
	if err != nil {
		pdk.Log(pdk.LogDebug, "cercle: getSong "+ev.Track.ID+" failed: "+err.Error())
		return
	}
	ev.Apply(en)
}

func songEnrichment(username, trackID string) (ingest.Enrichment, error) {
	key := songCachePrefix + trackID
	if s, ok, err := host.CacheGetString(key); err == nil && ok {
		if en, err := ingest.DecodeEnrichment(s); err == nil {
			return en, nil
		}
	}
	body, err := host.SubsonicAPICall(ingest.GetSongURI(username, trackID))
	if err != nil {
		return ingest.Enrichment{}, err
	}
	en, err := ingest.ParseGetSong(body)
	if err != nil {
		return ingest.Enrichment{}, err
	}
	_ = host.CacheSetString(key, ingest.EncodeEnrichment(en), songCacheTTL)
	return en, nil
}

// displayName returns the user's Navidrome display name, or "" when unknown.
func displayName(username string) string {
	if username == "" {
		return ""
	}
	key := userCachePrefix + username
	if name, ok, err := host.CacheGetString(key); err == nil && ok {
		return name
	}
	users, err := host.UsersGetUsers()
	if err != nil {
		return ""
	}
	found := ""
	for _, u := range users {
		_ = host.CacheSetString(userCachePrefix+u.UserName, u.Name, userCacheTTL)
		if u.UserName == username {
			found = u.Name
		}
	}
	if found == "" {
		_ = host.CacheSetString(key, "", userCacheTTL) // don't re-list users on every event
	}
	return found
}

func toTrack(t scrobbler.TrackInfo) ingest.Track {
	out := ingest.Track{
		ID:             t.ID,
		Title:          t.Title,
		Album:          t.Album,
		Artist:         t.Artist,
		AlbumArtist:    t.AlbumArtist,
		Duration:       t.Duration,
		TrackNumber:    t.TrackNumber,
		MBZRecordingID: t.MBZRecordingID,
		MBZAlbumID:     t.MBZAlbumID,
	}
	for _, a := range t.Artists {
		out.Artists = append(out.Artists, ingest.ArtistRef{ID: a.ID, Name: a.Name, MBID: a.MBID})
	}
	return out
}

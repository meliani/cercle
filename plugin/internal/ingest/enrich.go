package ingest

import (
	"encoding/json"
	"errors"
	"net/url"
)

// Enrichment holds the fields Navidrome does not pass to scrobbler plugins
// and that the plugin looks up with the Subsonic getSong endpoint.
type Enrichment struct {
	AlbumID  string `json:"album_id,omitempty"`
	ArtistID string `json:"artist_id,omitempty"`
	Year     int    `json:"year,omitempty"`
	Genre    string `json:"genre,omitempty"`
}

// Empty reports whether nothing useful was found.
func (e Enrichment) Empty() bool { return e == Enrichment{} }

// GetSongURI is the host Subsonic API call for one track, run as the given user.
func GetSongURI(username, trackID string) string {
	q := url.Values{}
	q.Set("u", username)
	q.Set("id", trackID)
	return "getSong?" + q.Encode()
}

type subsonicEnvelope struct {
	Response struct {
		Status string `json:"status"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Song *struct {
			AlbumID  string `json:"albumId"`
			ArtistID string `json:"artistId"`
			Year     int    `json:"year"`
			Genre    string `json:"genre"`
			Genres   []struct {
				Name string `json:"name"`
			} `json:"genres"`
			Artists []struct {
				ID string `json:"id"`
			} `json:"artists"`
		} `json:"song"`
	} `json:"subsonic-response"`
}

// ParseGetSong extracts the enrichment fields from a Subsonic JSON getSong response.
func ParseGetSong(body string) (Enrichment, error) {
	var env subsonicEnvelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		return Enrichment{}, err
	}
	r := env.Response
	if r.Status != "ok" {
		if r.Error != nil && r.Error.Message != "" {
			return Enrichment{}, errors.New("subsonic: " + r.Error.Message)
		}
		return Enrichment{}, errors.New("subsonic: status " + r.Status)
	}
	if r.Song == nil {
		return Enrichment{}, errors.New("subsonic: no song in response")
	}
	s := r.Song
	en := Enrichment{AlbumID: s.AlbumID, ArtistID: s.ArtistID, Year: s.Year, Genre: s.Genre}
	if en.ArtistID == "" && len(s.Artists) > 0 {
		en.ArtistID = s.Artists[0].ID
	}
	if en.Genre == "" && len(s.Genres) > 0 {
		en.Genre = s.Genres[0].Name
	}
	return en, nil
}

// Apply fills the track fields Navidrome left out. Ids already present (the
// artist id taken from the track's first artist) are kept.
func (e *Event) Apply(en Enrichment) {
	if e.Track.ArtistID == "" {
		e.Track.ArtistID = en.ArtistID
	}
	if e.Track.AlbumID == "" {
		e.Track.AlbumID = en.AlbumID
	}
	if e.Track.Year == 0 {
		e.Track.Year = en.Year
	}
	if e.Track.Genre == "" {
		e.Track.Genre = en.Genre
	}
}

// EncodeEnrichment / DecodeEnrichment serialise an Enrichment for the host cache.
func EncodeEnrichment(en Enrichment) string {
	b, _ := json.Marshal(en)
	return string(b)
}

func DecodeEnrichment(s string) (Enrichment, error) {
	var en Enrichment
	err := json.Unmarshal([]byte(s), &en)
	return en, err
}

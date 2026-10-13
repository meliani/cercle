package api

import (
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"strings"
	"testing"
)

func getRaw(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestSubsonicGetNowPlaying(t *testing.T) {
	e := newEnv(t)
	login(e, "alice", "bob", "carol")
	e.leaveEveryone("carol")
	e.ingest(nowPlayingEv("alice", track("t1", "al1", "ar1"), t0))

	type jsonResp struct {
		R struct {
			Status        string `json:"status"`
			Version       string `json:"version"`
			Type          string `json:"type"`
			ServerVersion string `json:"serverVersion"`
			OpenSubsonic  bool   `json:"openSubsonic"`
			Error         *struct {
				Code int `json:"code"`
			} `json:"error"`
			NowPlaying struct {
				Entry []struct {
					ID         string `json:"id"`
					Username   string `json:"username"`
					Duration   int64  `json:"duration"`
					CoverArt   string `json:"coverArt"`
					PlayerID   int    `json:"playerId"`
					PlayerName string `json:"playerName"`
					State      string `json:"state"`
					PositionMs int64  `json:"positionMs"`
					MinutesAgo int64  `json:"minutesAgo"`
					AlbumID    string `json:"albumId"`
				} `json:"entry"`
			} `json:"nowPlaying"`
		} `json:"subsonic-response"`
	}
	getJSON := func(url string) jsonResp {
		t.Helper()
		code, body := getRaw(t, url)
		if code != 200 {
			t.Fatalf("%s: HTTP %d", url, code)
		}
		var r jsonResp
		if err := json.Unmarshal([]byte(body), &r); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		return r
	}

	r := getJSON(e.ts.URL + "/hub/rest/getNowPlaying.view?u=bob&p=pw-bob&v=1.16.1&c=test&f=json")
	if r.R.Status != "ok" || r.R.Type != "cercle" || r.R.Version != "1.16.1" || !r.R.OpenSubsonic || r.R.ServerVersion != "test" {
		t.Fatalf("envelope: %+v", r.R)
	}
	if len(r.R.NowPlaying.Entry) != 1 {
		t.Fatalf("bob sees alice: %+v", r.R.NowPlaying)
	}
	en := r.R.NowPlaying.Entry[0]
	if en.ID != "t1" || en.Username != "alice" || en.Duration != 200 || en.CoverArt != "t1" || en.PlayerID != 1 ||
		en.PlayerName != "Feishin" || en.State != "playing" || en.PositionMs != 1000 || en.AlbumID != "al1" {
		t.Fatalf("entry: %+v", en)
	}

	// Credential cache: a second call does not hit Navidrome.
	calls := e.nav.callCount()
	getJSON(e.ts.URL + "/hub/rest/getNowPlaying.view?u=bob&p=pw-bob&f=json")
	if e.nav.callCount() != calls {
		t.Fatal("successful validation should be cached")
	}

	// Mounted at the root too; carol (no common circle) sees nothing.
	r = getJSON(e.ts.URL + "/rest/getNowPlaying?u=carol&p=pw-carol&f=json")
	if r.R.Status != "ok" || len(r.R.NowPlaying.Entry) != 0 {
		t.Fatalf("carol must not see alice: %+v", r.R.NowPlaying)
	}

	// Bad credentials → Subsonic error 40 with HTTP 200.
	r = getJSON(e.ts.URL + "/rest/getNowPlaying.view?u=bob&p=wrong&f=json")
	if r.R.Status != "failed" || r.R.Error == nil || r.R.Error.Code != 40 {
		t.Fatalf("bad creds: %+v", r.R)
	}

	// XML (default format).
	code, body := getRaw(t, e.ts.URL+"/hub/rest/getNowPlaying.view?u=bob&p=pw-bob")
	if code != 200 || !strings.Contains(body, `xmlns="http://subsonic.org/restapi"`) {
		t.Fatalf("xml: %d %s", code, body)
	}
	var x struct {
		XMLName    xml.Name `xml:"subsonic-response"`
		Status     string   `xml:"status,attr"`
		NowPlaying struct {
			Entry []struct {
				ID       string `xml:"id,attr"`
				Username string `xml:"username,attr"`
				PlayerID int    `xml:"playerId,attr"`
			} `xml:"entry"`
		} `xml:"nowPlaying"`
	}
	if err := xml.Unmarshal([]byte(body), &x); err != nil {
		t.Fatalf("xml decode: %v\n%s", err, body)
	}
	if x.Status != "ok" || len(x.NowPlaying.Entry) != 1 || x.NowPlaying.Entry[0].Username != "alice" || x.NowPlaying.Entry[0].PlayerID != 1 {
		t.Fatalf("xml shape: %+v\n%s", x, body)
	}
	_, body = getRaw(t, e.ts.URL+"/rest/getNowPlaying.view?u=carol&p=pw-carol&f=xml")
	if strings.Contains(body, "alice") {
		t.Fatalf("carol xml leaks alice: %s", body)
	}
	_, body = getRaw(t, e.ts.URL+"/rest/getNowPlaying.view?u=bob&p=nope")
	if !strings.Contains(body, `status="failed"`) || !strings.Contains(body, `code="40"`) {
		t.Fatalf("xml error: %s", body)
	}

	// POST form works too.
	resp, err := http.PostForm(e.ts.URL+"/hub/rest/getNowPlaying", map[string][]string{"u": {"bob"}, "p": {"pw-bob"}, "f": {"json"}})
	if err != nil {
		t.Fatal(err)
	}
	var pr jsonResp
	_ = json.NewDecoder(resp.Body).Decode(&pr)
	resp.Body.Close()
	if len(pr.R.NowPlaying.Entry) != 1 {
		t.Fatalf("POST: %+v", pr.R)
	}
}

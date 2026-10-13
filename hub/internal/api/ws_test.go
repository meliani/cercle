package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type wsMsg struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

func (e *env) dial(user string) *websocket.Conn {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(e.ts.URL, "http") + "/hub/api/v1/ws?token=" + e.token(user)
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		e.t.Fatalf("dial %s: %v", user, err)
	}
	e.t.Cleanup(func() { c.CloseNow() })
	if m := readMsg(e.t, c); m.Type != "hello" {
		e.t.Fatalf("first message %q, want hello", m.Type)
	}
	return c
}

func readMsg(t *testing.T, c *websocket.Conn) wsMsg {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m wsMsg
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func send(t *testing.T, c *websocket.Conn, typ string, data any) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"type": typ, "data": data})
	if err := c.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

// expectPongNext proves nothing else was queued for c: messages are ordered
// per connection, so if the reply to a ping comes first, no event was sent.
func expectPongNext(t *testing.T, c *websocket.Conn, why string) {
	t.Helper()
	send(t, c, "ping", map[string]any{})
	if m := readMsg(t, c); m.Type != "pong" {
		t.Fatalf("%s: got %q (%s) before pong", why, m.Type, m.Data)
	}
}

func TestWSVisibilityFiltering(t *testing.T) {
	e := newEnv(t)
	login(e, "alice", "bob", "carol")
	e.leaveEveryone("carol")
	bob := e.dial("bob")
	carol := e.dial("carol")

	e.ingest(nowPlayingEv("alice", track("t1", "al1", "ar1"), t0))
	m := readMsg(t, bob)
	if m.Type != "live" || !strings.Contains(string(m.Data), `"username":"alice"`) {
		t.Fatalf("bob should receive alice's live: %s %s", m.Type, m.Data)
	}
	expectPongNext(t, carol, "carol must not receive alice's live")

	e.scrobble("alice", track("t1", "al1", "ar1"), t0.Add(-time.Minute))
	got := map[string]bool{}
	for i := 0; i < 2; i++ {
		m := readMsg(t, bob)
		if m.Type != "feed_item" {
			t.Fatalf("bob: %s", m.Type)
		}
		var it struct {
			Kind string `json:"kind"`
		}
		_ = json.Unmarshal(m.Data, &it)
		got[it.Kind] = true
	}
	if !got["listened"] || !got["discovery_artist"] {
		t.Fatalf("bob feed items: %v", got)
	}
	expectPongNext(t, carol, "carol must not receive alice's feed items")

	// Pausing ends the live state for others.
	e.must(200, "POST", "/api/v1/me/pause", "alice", map[string]int{"minutes": 30}, nil)
	if m := readMsg(t, bob); m.Type != "live_end" {
		t.Fatalf("bob after pause: %s", m.Type)
	}
	expectPongNext(t, carol, "carol is a stranger: not even live_end")

	// Room subscriptions need listener status.
	var rm roomResp
	e.must(201, "POST", "/api/v1/circles/everyone/rooms", "alice", map[string]string{"name": "R"}, &rm)
	if m := readMsg(t, bob); m.Type != "room_update" {
		t.Fatalf("circle members get room_update: %s", m.Type)
	}
	send(t, bob, "subscribe_room", map[string]string{"room_id": rm.ID})
	if m := readMsg(t, bob); m.Type != "error" {
		t.Fatalf("subscribe without joining: %s", m.Type)
	}
	e.must(200, "POST", "/api/v1/rooms/"+rm.ID+"/join", "bob", nil, nil)
	if m := readMsg(t, bob); m.Type != "room_update" {
		t.Fatalf("join → room_update: %s", m.Type)
	}
	send(t, bob, "subscribe_room", map[string]string{"room_id": rm.ID})
	if m := readMsg(t, bob); m.Type != "room_state" {
		t.Fatalf("subscribe sends current state: %s", m.Type)
	}
	e.must(200, "PUT", "/api/v1/rooms/"+rm.ID+"/state", "alice", map[string]any{"track_id": "t5", "playing": true}, nil)
	m = readMsg(t, bob)
	if m.Type != "room_state" || !strings.Contains(string(m.Data), `"seq":1`) {
		t.Fatalf("room_state: %s %s", m.Type, m.Data)
	}
	expectPongNext(t, carol, "carol gets no room events")
}

func TestWSAuthAndTimeout(t *testing.T) {
	e := newEnv(t)
	login(e, "alice")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(e.ts.URL, "http")+"/hub/api/v1/ws?token=nope", nil)
	if err == nil || resp == nil || resp.StatusCode != 401 {
		t.Fatalf("bad token must be rejected with 401: %v", err)
	}

	e.srv.Hub.PingInterval = 50 * time.Millisecond
	e.srv.Hub.Timeout = 200 * time.Millisecond
	c := e.dial("alice")
	// Not reading means pings go unanswered; the server drops us.
	time.Sleep(600 * time.Millisecond)
	rctx, rcancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer rcancel()
	if _, _, err := c.Read(rctx); err == nil {
		t.Fatal("silent connection should have been dropped")
	}
	deadline := time.Now().Add(2 * time.Second)
	for e.srv.Hub.Count() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := e.srv.Hub.Count(); n != 0 {
		t.Fatalf("connection still registered: %d", n)
	}
}

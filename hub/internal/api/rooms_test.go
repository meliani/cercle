package api

import (
	"context"
	"testing"
	"time"

	"github.com/meliani/cercle/hub/internal/rooms"
)

type roomResp struct {
	ID   string `json:"id"`
	Host struct {
		Username string `json:"username"`
	} `json:"host"`
	Listeners []struct {
		Username string `json:"username"`
	} `json:"listeners"`
	State struct {
		TrackID *string  `json:"track_id"`
		Seq     int64    `json:"seq"`
		Queue   []string `json:"queue"`
		Playing bool     `json:"playing"`
	} `json:"state"`
	Requests []struct {
		ID string `json:"id"`
	} `json:"requests"`
}

func TestRoomsHostTransferOnLeave(t *testing.T) {
	e := newEnv(t)
	login(e, "alice", "bob", "carol", "dave")
	e.leaveEveryone("dave")
	var rm roomResp
	e.must(201, "POST", "/api/v1/circles/everyone/rooms", "alice", map[string]string{"name": "Friday"}, &rm)
	if rm.Host.Username != "alice" || len(rm.Listeners) != 1 {
		t.Fatalf("creator hosts and listens: %+v", rm)
	}
	id := rm.ID
	if code := e.req("POST", "/api/v1/rooms/"+id+"/join", "dave", nil, nil); code != 404 {
		t.Fatalf("non-member join: %d", code)
	}
	e.must(200, "POST", "/api/v1/rooms/"+id+"/join", "bob", nil, nil)
	e.clock.Advance(time.Second)
	e.must(200, "POST", "/api/v1/rooms/"+id+"/join", "carol", nil, &rm)
	if len(rm.Listeners) != 3 {
		t.Fatalf("listeners: %+v", rm.Listeners)
	}

	// State: host only, seq increments.
	if code := e.req("PUT", "/api/v1/rooms/"+id+"/state", "bob", map[string]any{"track_id": "t1", "playing": true}, nil); code != 403 {
		t.Fatalf("non-host state: %d", code)
	}
	var st struct {
		Seq int64 `json:"seq"`
	}
	e.must(200, "PUT", "/api/v1/rooms/"+id+"/state", "alice", map[string]any{"track_id": "t1", "position_ms": 5000, "playing": true, "queue": []string{"t2"}}, &st)
	e.must(200, "PUT", "/api/v1/rooms/"+id+"/state", "alice", map[string]any{"track_id": "t1", "position_ms": 9000, "playing": false}, &st)
	if st.Seq != 2 {
		t.Fatalf("seq: %d", st.Seq)
	}

	// Requests.
	var req struct {
		ID string `json:"id"`
	}
	if code := e.req("POST", "/api/v1/rooms/"+id+"/requests", "dave", map[string]any{"track": map[string]string{"id": "t7"}}, nil); code != 404 {
		t.Fatalf("non-member request: %d", code)
	}
	e.must(201, "POST", "/api/v1/rooms/"+id+"/requests", "carol", map[string]any{"track": map[string]any{"id": "t7", "title": "Seven"}}, &req)
	if code := e.req("DELETE", "/api/v1/rooms/"+id+"/requests/"+req.ID, "bob", nil, nil); code != 403 {
		t.Fatalf("other listener deletes request: %d", code)
	}
	e.must(201, "POST", "/api/v1/rooms/"+id+"/requests", "carol", map[string]any{"track": map[string]any{"id": "t8"}}, nil)
	e.must(204, "DELETE", "/api/v1/rooms/"+id+"/requests/"+req.ID, "carol", nil, nil)
	e.must(200, "GET", "/api/v1/rooms/"+id, "bob", nil, &rm)
	if len(rm.Requests) != 1 {
		t.Fatalf("requests: %+v", rm.Requests)
	}

	// Persistence: a fresh manager reloads the same room.
	m2 := rooms.New(e.srv.DB, e.clock.Now)
	if err := m2.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r2, ok := m2.Get(id); !ok || r2.State.Seq != 2 || len(r2.Listeners) != 3 || len(r2.Requests) != 1 || *r2.State.TrackID != "t1" {
		t.Fatalf("persisted room: %+v", r2)
	}

	if code := e.req("PUT", "/api/v1/rooms/"+id+"/host", "alice", map[string]string{"username": "dave"}, nil); code != 400 {
		t.Fatalf("host must be a listener: %d", code)
	}

	e.must(204, "POST", "/api/v1/rooms/"+id+"/leave", "alice", nil, nil)
	e.must(200, "GET", "/api/v1/rooms/"+id, "bob", nil, &rm)
	if rm.Host.Username != "bob" {
		t.Fatalf("host passes to the longest listener: %q", rm.Host.Username)
	}
	e.must(204, "POST", "/api/v1/rooms/"+id+"/leave", "bob", nil, nil)
	e.must(200, "GET", "/api/v1/rooms/"+id, "carol", nil, &rm)
	if rm.Host.Username != "carol" {
		t.Fatalf("then to carol: %q", rm.Host.Username)
	}
	e.must(204, "POST", "/api/v1/rooms/"+id+"/leave", "carol", nil, nil)
	if code := e.req("GET", "/api/v1/rooms/"+id, "carol", nil, nil); code != 404 {
		t.Fatalf("empty room is closed: %d", code)
	}
	var n int
	_ = e.srv.DB.R.QueryRow(`SELECT COUNT(*) FROM rooms`).Scan(&n)
	if n != 0 {
		t.Fatal("closed room removed from the database")
	}
}

func TestRoomDeletePermissions(t *testing.T) {
	e := newEnv(t)
	login(e, "alice", "bob", "carol")
	fam := e.createCircle("alice", "Family", "bob", "carol")
	var rm roomResp
	e.must(201, "POST", "/api/v1/circles/"+fam+"/rooms", "bob", map[string]string{"name": "Bob's"}, &rm)
	e.must(200, "POST", "/api/v1/rooms/"+rm.ID+"/join", "carol", nil, nil)
	if code := e.req("DELETE", "/api/v1/rooms/"+rm.ID, "carol", nil, nil); code != 403 {
		t.Fatalf("listener delete: %d", code)
	}
	var list struct {
		Items []roomResp `json:"items"`
	}
	e.must(200, "GET", "/api/v1/circles/"+fam+"/rooms", "carol", nil, &list)
	if len(list.Items) != 1 {
		t.Fatalf("rooms list: %+v", list)
	}
	e.must(204, "DELETE", "/api/v1/rooms/"+rm.ID, "alice", nil, nil) // circle owner
	e.must(200, "GET", "/api/v1/circles/"+fam+"/rooms", "carol", nil, &list)
	if len(list.Items) != 0 {
		t.Fatal("deleted")
	}
	// Leaving the circle removes you from its rooms.
	e.must(201, "POST", "/api/v1/circles/"+fam+"/rooms", "bob", map[string]string{"name": "Again"}, &rm)
	e.must(200, "POST", "/api/v1/rooms/"+rm.ID+"/join", "carol", nil, nil)
	e.must(204, "POST", "/api/v1/circles/"+fam+"/leave", "bob", nil, nil)
	e.must(200, "GET", "/api/v1/rooms/"+rm.ID, "carol", nil, &rm)
	if rm.Host.Username != "carol" || len(rm.Listeners) != 1 {
		t.Fatalf("bob left the circle → left the room: %+v", rm)
	}
}

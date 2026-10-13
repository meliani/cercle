package ws

import (
	"io"
	"log/slog"
	"testing"

	"github.com/meliani/cercle/hub/internal/events"
)

func testConn(h *Hub, user string, queue int) *conn {
	c := &conn{hub: h, user: user, send: make(chan []byte, queue), done: make(chan struct{}), rooms: map[string]bool{}}
	h.mu.Lock()
	h.conns[c] = struct{}{}
	h.mu.Unlock()
	return c
}

func closed(c *conn) bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

func TestPublishFiltersAndDropsSlowConsumers(t *testing.T) {
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	alice := testConn(h, "alice", 2)
	bob := testConn(h, "bob", 2)
	ev := events.Event{Type: "live", Allow: func(v string) bool { return v == "alice" }, Data: events.Same(map[string]int{"n": 1})}
	h.Publish(ev)
	if len(alice.send) != 1 || len(bob.send) != 0 {
		t.Fatalf("filter: alice=%d bob=%d", len(alice.send), len(bob.send))
	}
	h.Publish(ev)
	h.Publish(ev) // queue full → connection dropped, publisher not blocked
	if !closed(alice) {
		t.Fatal("slow consumer must be dropped")
	}
	if closed(bob) {
		t.Fatal("bob untouched")
	}
	// Room events only reach subscribers.
	bob.rooms["r1"] = true
	h.Publish(events.Event{Type: "room_state", Room: "r1", Allow: func(string) bool { return true }, Data: events.Same(1)})
	h.Publish(events.Event{Type: "room_state", Room: "r2", Allow: func(string) bool { return true }, Data: events.Same(1)})
	if len(bob.send) != 1 {
		t.Fatalf("room filter: %d", len(bob.send))
	}
	// nil Allow delivers to nobody.
	h.Publish(events.Event{Type: "x", Data: events.Same(1)})
	if len(bob.send) != 1 {
		t.Fatal("nil Allow must deliver nothing")
	}
}

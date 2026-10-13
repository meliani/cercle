// Package events is the in-process fan-out contract between the services
// that change state (ingest, feed, rooms, live) and the WebSocket hub.
package events

import "sync"

// Event is one server → client message. The publisher decides the audience
// (via the visibility rule) before publishing, so the WebSocket hub never
// needs to know about circles or privacy.
type Event struct {
	Type string
	// Allow reports whether the connection of user viewer may receive the
	// event. A nil Allow delivers to nobody.
	Allow func(viewer string) bool
	// Data renders the payload for viewer (e.g. reactions carry "mine").
	Data func(viewer string) any
	// Room, when set, restricts delivery to connections subscribed to it.
	Room string
}

// Publisher accepts events. Publish must not block on slow consumers.
type Publisher interface {
	Publish(Event)
}

// Nop discards everything (used by tests and when no hub is wired).
type Nop struct{}

func (Nop) Publish(Event) {}

// Same returns a Data func that renders v for every viewer.
func Same(v any) func(string) any { return func(string) any { return v } }

// Set returns an Allow func backed by a set.
func Set(m map[string]bool) func(string) bool { return func(u string) bool { return m[u] } }

// Recorder collects events (tests).
type Recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *Recorder) Publish(e Event) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

// Events returns a copy of what was published so far.
func (r *Recorder) Events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

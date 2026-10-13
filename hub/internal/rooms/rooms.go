// Package rooms keeps listen-together rooms in memory and persists every
// change to SQLite. Permission checks that need circle roles are done by the
// caller (package api); this package enforces room-local rules (host only,
// listener only) and the host hand-over on leave.
package rooms

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/meliani/cercle/hub/internal/db"
	"github.com/meliani/cercle/hub/internal/model"
)

// State is the synchronised playback state.
type State struct {
	TrackID    *string  `json:"track_id"`
	PositionMs int64    `json:"position_ms"`
	Playing    bool     `json:"playing"`
	Queue      []string `json:"queue"`
	UpdatedAt  int64    `json:"updated_at"`
	Seq        int64    `json:"seq"`
}

// Wire renders the state.
func (s State) Wire() model.RoomState {
	q := s.Queue
	if q == nil {
		q = []string{}
	}
	return model.RoomState{TrackID: s.TrackID, PositionMs: s.PositionMs, Playing: s.Playing, Queue: q,
		UpdatedAt: model.TS(s.UpdatedAt), Seq: s.Seq}
}

type Listener struct {
	User     string
	JoinedAt int64
}

type Request struct {
	ID        string
	User      string
	Track     model.Track
	CreatedAt int64
}

// Room is a snapshot (copies, safe to read without the lock).
type Room struct {
	ID        string
	CircleID  string
	Name      string
	Host      string
	CreatedAt int64
	Listeners []Listener
	Requests  []Request
	State     State
}

func (r *Room) clone() Room {
	c := *r
	c.Listeners = append([]Listener(nil), r.Listeners...)
	c.Requests = append([]Request(nil), r.Requests...)
	c.State.Queue = append([]string(nil), r.State.Queue...)
	return c
}

// IsListener reports whether u is in the room.
func (r Room) IsListener(u string) bool {
	for _, l := range r.Listeners {
		if l.User == u {
			return true
		}
	}
	return false
}

// Manager owns all rooms.
type Manager struct {
	DB  *db.DB
	Now func() time.Time

	mu    sync.Mutex
	rooms map[string]*Room
}

func New(d *db.DB, now func() time.Time) *Manager {
	return &Manager{DB: d, Now: now, rooms: map[string]*Room{}}
}

func newID() string {
	b := make([]byte, 9)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Load reads persisted rooms (call once at start).
func (m *Manager) Load(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows, err := m.DB.R.QueryContext(ctx, `SELECT id, circle_id, name, host, created_at, state, seq FROM rooms`)
	if err != nil {
		return err
	}
	for rows.Next() {
		r := &Room{}
		var st string
		if err := rows.Scan(&r.ID, &r.CircleID, &r.Name, &r.Host, &r.CreatedAt, &st, &r.State.Seq); err != nil {
			rows.Close()
			return err
		}
		seq := r.State.Seq
		_ = json.Unmarshal([]byte(st), &r.State)
		r.State.Seq = seq
		m.rooms[r.ID] = r
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	lr, err := m.DB.R.QueryContext(ctx, `SELECT room_id, user_id, joined_at FROM room_listeners ORDER BY joined_at, user_id`)
	if err != nil {
		return err
	}
	for lr.Next() {
		var id string
		var l Listener
		if err := lr.Scan(&id, &l.User, &l.JoinedAt); err != nil {
			lr.Close()
			return err
		}
		if r := m.rooms[id]; r != nil {
			r.Listeners = append(r.Listeners, l)
		}
	}
	lr.Close()
	if err := lr.Err(); err != nil {
		return err
	}
	rr, err := m.DB.R.QueryContext(ctx, `SELECT id, room_id, user_id, track, created_at FROM room_requests ORDER BY created_at, id`)
	if err != nil {
		return err
	}
	defer rr.Close()
	for rr.Next() {
		var q Request
		var room, track string
		if err := rr.Scan(&q.ID, &room, &q.User, &track, &q.CreatedAt); err != nil {
			return err
		}
		_ = json.Unmarshal([]byte(track), &q.Track)
		if r := m.rooms[room]; r != nil {
			r.Requests = append(r.Requests, q)
		}
	}
	return rr.Err()
}

// persist writes the whole room (small) in one transaction. Caller holds mu.
func (m *Manager) persist(ctx context.Context, r *Room) error {
	st, _ := json.Marshal(r.State)
	return m.DB.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO rooms (id, circle_id, name, host, created_at, state, seq)
			VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO UPDATE SET name = excluded.name, host = excluded.host,
			state = excluded.state, seq = excluded.seq`,
			r.ID, r.CircleID, r.Name, r.Host, r.CreatedAt, string(st), r.State.Seq); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM room_listeners WHERE room_id = ?`, r.ID); err != nil {
			return err
		}
		for _, l := range r.Listeners {
			if _, err := tx.ExecContext(ctx, `INSERT INTO room_listeners (room_id, user_id, joined_at) VALUES (?, ?, ?)`,
				r.ID, l.User, l.JoinedAt); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM room_requests WHERE room_id = ?`, r.ID); err != nil {
			return err
		}
		for _, q := range r.Requests {
			t, _ := json.Marshal(q.Track)
			if _, err := tx.ExecContext(ctx, `INSERT INTO room_requests (id, room_id, user_id, track, created_at) VALUES (?, ?, ?, ?, ?)`,
				q.ID, r.ID, q.User, string(t), q.CreatedAt); err != nil {
				return err
			}
		}
		return nil
	})
}

func (m *Manager) remove(ctx context.Context, id string) error {
	delete(m.rooms, id)
	_, err := m.DB.W.ExecContext(ctx, `DELETE FROM rooms WHERE id = ?`, id)
	return err
}

// Get returns a snapshot.
func (m *Manager) Get(id string) (Room, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rooms[id]
	if !ok {
		return Room{}, false
	}
	return r.clone(), true
}

// ByCircle lists the circle's rooms, oldest first.
func (m *Manager) ByCircle(circleID string) []Room {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Room
	for _, r := range m.rooms {
		if r.CircleID == circleID {
			out = append(out, r.clone())
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Create opens a room hosted (and joined) by host.
func (m *Manager) Create(ctx context.Context, circleID, name, host string) (Room, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.Now().UnixMilli()
	r := &Room{ID: newID(), CircleID: circleID, Name: name, Host: host, CreatedAt: now,
		Listeners: []Listener{{User: host, JoinedAt: now}}, State: State{Queue: []string{}, UpdatedAt: now}}
	if err := m.persist(ctx, r); err != nil {
		return Room{}, err
	}
	m.rooms[r.ID] = r
	return r.clone(), nil
}

// Join adds u as a listener (idempotent).
func (m *Manager) Join(ctx context.Context, id, u string) (Room, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rooms[id]
	if !ok {
		return Room{}, model.ErrNotFound
	}
	if !r.IsListener(u) {
		r.Listeners = append(r.Listeners, Listener{User: u, JoinedAt: m.Now().UnixMilli()})
		if err := m.persist(ctx, r); err != nil {
			return Room{}, err
		}
	}
	return r.clone(), nil
}

// Leave removes u. If u was host, the longest-present listener becomes host;
// if the room becomes empty it is closed (closed = true).
func (m *Manager) Leave(ctx context.Context, id, u string) (room Room, closed bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rooms[id]
	if !ok {
		return Room{}, false, model.ErrNotFound
	}
	if !r.IsListener(u) {
		return r.clone(), false, nil
	}
	kept := r.Listeners[:0]
	for _, l := range r.Listeners {
		if l.User != u {
			kept = append(kept, l)
		}
	}
	r.Listeners = kept
	if len(r.Listeners) == 0 {
		snap := r.clone()
		return snap, true, m.remove(ctx, id)
	}
	if r.Host == u {
		next := r.Listeners[0]
		for _, l := range r.Listeners[1:] {
			if l.JoinedAt < next.JoinedAt {
				next = l
			}
		}
		r.Host = next.User
	}
	return r.clone(), false, m.persist(ctx, r)
}

// SetState replaces the playback state (host only) and bumps seq.
func (m *Manager) SetState(ctx context.Context, id, u string, st State) (Room, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rooms[id]
	if !ok {
		return Room{}, model.ErrNotFound
	}
	if r.Host != u {
		return Room{}, model.ErrForbidden
	}
	if st.Queue == nil {
		st.Queue = []string{}
	}
	if st.PositionMs < 0 {
		st.PositionMs = 0
	}
	st.Seq = r.State.Seq + 1
	st.UpdatedAt = m.Now().UnixMilli()
	r.State = st
	return r.clone(), m.persist(ctx, r)
}

// SetHost transfers hosting (host only) to a current listener.
func (m *Manager) SetHost(ctx context.Context, id, u, to string) (Room, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rooms[id]
	if !ok {
		return Room{}, model.ErrNotFound
	}
	if r.Host != u {
		return Room{}, model.ErrForbidden
	}
	if !r.IsListener(to) {
		return Room{}, model.ErrInvalid
	}
	r.Host = to
	return r.clone(), m.persist(ctx, r)
}

// AddRequest queues a track request (listeners only).
func (m *Manager) AddRequest(ctx context.Context, id, u string, t model.Track) (Room, Request, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rooms[id]
	if !ok {
		return Room{}, Request{}, model.ErrNotFound
	}
	if !r.IsListener(u) {
		return Room{}, Request{}, model.ErrForbidden
	}
	if len(r.Requests) >= 200 {
		return Room{}, Request{}, model.ErrConflict
	}
	q := Request{ID: newID(), User: u, Track: t, CreatedAt: m.Now().UnixMilli()}
	r.Requests = append(r.Requests, q)
	return r.clone(), q, m.persist(ctx, r)
}

// DeleteRequest removes a request (host or requester).
func (m *Manager) DeleteRequest(ctx context.Context, id, u, rid string) (Room, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rooms[id]
	if !ok {
		return Room{}, model.ErrNotFound
	}
	idx := -1
	for i, q := range r.Requests {
		if q.ID == rid {
			idx = i
		}
	}
	if idx < 0 {
		return Room{}, model.ErrNotFound
	}
	if r.Host != u && r.Requests[idx].User != u {
		return Room{}, model.ErrForbidden
	}
	r.Requests = append(r.Requests[:idx], r.Requests[idx+1:]...)
	return r.clone(), m.persist(ctx, r)
}

// Close deletes the room (permission checked by caller).
func (m *Manager) Close(ctx context.Context, id string) (Room, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rooms[id]
	if !ok {
		return Room{}, model.ErrNotFound
	}
	snap := r.clone()
	return snap, m.remove(ctx, id)
}

// Forget drops in-memory rooms of a circle whose rows were already deleted
// (circle deletion cascades in SQL). Returns the dropped rooms.
func (m *Manager) Forget(circleID string) []Room {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Room
	for id, r := range m.rooms {
		if r.CircleID == circleID {
			out = append(out, r.clone())
			delete(m.rooms, id)
		}
	}
	return out
}

// RoomsWith lists rooms in circleID where u is a listener.
func (m *Manager) RoomsWith(circleID, u string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for id, r := range m.rooms {
		if r.CircleID == circleID && r.IsListener(u) {
			out = append(out, id)
		}
	}
	return out
}

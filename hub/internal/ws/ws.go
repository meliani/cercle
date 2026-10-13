// Package ws is the WebSocket fan-out hub (docs/CONTRACT.md §5). It knows
// nothing about circles or privacy: every events.Event carries its own
// per-recipient Allow func computed by the publisher via the visibility rule.
package ws

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/meliani/cercle/hub/internal/events"
)

// Message is the wire envelope.
type Message struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

type inbound struct {
	Type string `json:"type"`
	Data struct {
		RoomID string `json:"room_id"`
	} `json:"data"`
}

// Hub tracks connections.
type Hub struct {
	// Authenticate resolves the ?token= (or bearer) to a username.
	Authenticate func(ctx context.Context, r *http.Request) (string, error)
	// Hello renders the hello payload ({user: Me}).
	Hello func(ctx context.Context, user string) (any, error)
	// CanSubscribe reports whether user is a listener of the room.
	CanSubscribe func(roomID, user string) bool
	// RoomState returns the current room_state payload sent on subscribe.
	RoomState func(roomID string) (any, bool)
	// AllowOrigin checks the browser Origin header (empty origin = non-browser).
	AllowOrigin func(r *http.Request) bool

	PingInterval time.Duration
	Timeout      time.Duration
	QueueSize    int
	Log          *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.RWMutex
	conns  map[*conn]struct{}
}

func New(log *slog.Logger) *Hub {
	ctx, cancel := context.WithCancel(context.Background())
	return &Hub{PingInterval: 30 * time.Second, Timeout: 75 * time.Second, QueueSize: 64, Log: log,
		ctx: ctx, cancel: cancel, conns: map[*conn]struct{}{}}
}

type conn struct {
	hub      *Hub
	user     string
	ws       *websocket.Conn
	send     chan []byte
	done     chan struct{}
	once     sync.Once
	lastSeen atomic.Int64

	mu    sync.Mutex
	rooms map[string]bool
}

func (c *conn) touch() { c.lastSeen.Store(time.Now().UnixMilli()) }

func (c *conn) kill(code websocket.StatusCode, reason string) {
	c.once.Do(func() {
		close(c.done)
		if c.ws != nil {
			go c.ws.Close(code, reason)
		}
	})
}

func (c *conn) enqueue(b []byte) {
	select {
	case <-c.done:
	case c.send <- b:
	default:
		c.hub.Log.Warn("ws slow consumer dropped", "user", c.user)
		c.kill(websocket.StatusPolicyViolation, "slow consumer")
	}
}

func (c *conn) subscribed(room string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rooms[room]
}

func marshal(typ string, data any) []byte {
	b, err := json.Marshal(Message{Type: typ, Data: data})
	if err != nil {
		return nil
	}
	return b
}

// Count returns the number of open connections.
func (h *Hub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.conns)
}

// Publish implements events.Publisher; it never blocks.
func (h *Hub) Publish(ev events.Event) {
	if ev.Allow == nil || ev.Data == nil {
		return
	}
	h.mu.RLock()
	list := make([]*conn, 0, len(h.conns))
	for c := range h.conns {
		list = append(list, c)
	}
	h.mu.RUnlock()
	cache := map[string][]byte{}
	for _, c := range list {
		if ev.Room != "" && !c.subscribed(ev.Room) {
			continue
		}
		if !ev.Allow(c.user) {
			continue
		}
		b, ok := cache[c.user]
		if !ok {
			b = marshal(ev.Type, ev.Data(c.user))
			cache[c.user] = b
		}
		if b != nil {
			c.enqueue(b)
		}
	}
}

// Unsubscribe removes room subscriptions of user's connections.
func (h *Hub) Unsubscribe(roomID, user string) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.conns {
		if c.user == user {
			c.mu.Lock()
			delete(c.rooms, roomID)
			c.mu.Unlock()
		}
	}
}

// Close drops every connection (graceful shutdown).
func (h *Hub) Close() {
	h.cancel()
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.conns {
		c.kill(websocket.StatusGoingAway, "server shutting down")
	}
}

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func httpErr(w http.ResponseWriter, status int, code, msg string) {
	var e errorBody
	e.Error.Code, e.Error.Message = code, msg
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(e)
}

// ServeHTTP upgrades GET /api/v1/ws?token=….
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	user, err := h.Authenticate(r.Context(), r)
	if err != nil || user == "" {
		httpErr(w, http.StatusUnauthorized, "unauthorized", "invalid or missing token")
		return
	}
	if h.AllowOrigin != nil && !h.AllowOrigin(r) {
		httpErr(w, http.StatusForbidden, "forbidden", "origin not allowed")
		return
	}
	hello, err := h.Hello(r.Context(), user)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, "internal", "could not load user")
		return
	}
	// Origin was checked above against CERCLE_CORS_ORIGINS / same host.
	wsc, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	wsc.SetReadLimit(64 << 10)
	c := &conn{hub: h, user: user, ws: wsc, send: make(chan []byte, h.QueueSize), done: make(chan struct{}),
		rooms: map[string]bool{}}
	c.touch()
	h.mu.Lock()
	h.conns[c] = struct{}{}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.conns, c)
		h.mu.Unlock()
		c.kill(websocket.StatusNormalClosure, "")
	}()
	if h.ctx.Err() != nil {
		return
	}
	c.enqueue(marshal("hello", hello))

	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	go func() {
		select {
		case <-c.done:
			cancel()
		case <-ctx.Done():
		}
	}()
	go h.writeLoop(ctx, c)
	go h.pingLoop(ctx, c)
	h.readLoop(ctx, c)
}

func (h *Hub) writeLoop(ctx context.Context, c *conn) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		case b := <-c.send:
			wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := c.ws.Write(wctx, websocket.MessageText, b)
			cancel()
			if err != nil {
				c.kill(websocket.StatusGoingAway, "write failed")
				return
			}
		}
	}
}

func (h *Hub) pingLoop(ctx context.Context, c *conn) {
	t := time.NewTicker(h.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if time.Since(time.UnixMilli(c.lastSeen.Load())) > h.Timeout {
				c.kill(websocket.StatusPolicyViolation, "timeout")
				return
			}
			go func() {
				pctx, cancel := context.WithTimeout(ctx, h.Timeout)
				defer cancel()
				if c.ws.Ping(pctx) == nil {
					c.touch()
				}
			}()
		}
	}
}

func (h *Hub) readLoop(ctx context.Context, c *conn) {
	for {
		typ, data, err := c.ws.Read(ctx)
		if err != nil {
			if !errors.Is(err, context.Canceled) && websocket.CloseStatus(err) == -1 {
				h.Log.Debug("ws read", "user", c.user, "err", err)
			}
			return
		}
		c.touch()
		if typ != websocket.MessageText {
			continue
		}
		var in inbound
		if json.Unmarshal(data, &in) != nil {
			c.enqueue(marshal("error", map[string]string{"code": "bad_request", "message": "invalid JSON"}))
			continue
		}
		switch in.Type {
		case "ping":
			c.enqueue(marshal("pong", struct{}{}))
		case "subscribe_room":
			if in.Data.RoomID == "" || h.CanSubscribe == nil || !h.CanSubscribe(in.Data.RoomID, c.user) {
				c.enqueue(marshal("error", map[string]string{"code": "forbidden", "message": "not a listener of this room"}))
				continue
			}
			c.mu.Lock()
			c.rooms[in.Data.RoomID] = true
			c.mu.Unlock()
			if h.RoomState != nil {
				if st, ok := h.RoomState(in.Data.RoomID); ok {
					c.enqueue(marshal("room_state", st))
				}
			}
		case "unsubscribe_room":
			c.mu.Lock()
			delete(c.rooms, in.Data.RoomID)
			c.mu.Unlock()
		default:
			c.enqueue(marshal("error", map[string]string{"code": "bad_request", "message": "unknown message type"}))
		}
	}
}

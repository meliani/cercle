// Package live keeps each user's "now playing" state in memory and fans
// changes out through the visibility rule.
package live

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/meliani/cercle/hub/internal/events"
	"github.com/meliani/cercle/hub/internal/model"
	"github.com/meliani/cercle/hub/internal/users"
	"github.com/meliani/cercle/hub/internal/visibility"
)

// ExpiryGrace is added to the track duration before a silent state expires.
const ExpiryGrace = 60 * time.Second

// State is one user's live state.
type State struct {
	User       string
	Track      model.Track
	State      string // "playing" | "paused"
	PositionMs int64
	StartedAt  int64 // unix ms
	UpdatedAt  int64 // unix ms
	PlayerID   string
	PlayerName string
}

func (s State) expired(now int64) bool {
	return now > s.UpdatedAt+s.Track.DurationMs+ExpiryGrace.Milliseconds()
}

// Service owns the live states.
type Service struct {
	Vis   *visibility.V
	Users *users.Service
	Pub   events.Publisher
	Now   func() time.Time
	Log   *slog.Logger

	mu sync.Mutex
	m  map[string]State
}

func New(vis *visibility.V, us *users.Service, pub events.Publisher, now func() time.Time, log *slog.Logger) *Service {
	return &Service{Vis: vis, Users: us, Pub: pub, Now: now, Log: log, m: map[string]State{}}
}

// NowPlaying sets the user's live state (playing, from the event).
func (s *Service) NowPlaying(ctx context.Context, user string, t model.Track, startedAt, pos int64, playerID, playerName string) {
	now := s.Now().UnixMilli()
	s.mu.Lock()
	s.m[user] = State{User: user, Track: t, State: "playing", PositionMs: pos, StartedAt: startedAt,
		UpdatedAt: now, PlayerID: playerID, PlayerName: playerName}
	s.mu.Unlock()
	s.Refresh(ctx, user)
}

// PlaybackState applies a playback_state event. state is already normalised
// to playing|paused|stopped. t may be nil when the event carries no track.
func (s *Service) PlaybackState(ctx context.Context, user, state string, t *model.Track, pos *int64, startedAt int64, playerID, playerName string) {
	now := s.Now().UnixMilli()
	s.mu.Lock()
	cur, ok := s.m[user]
	if ok && cur.expired(now) {
		ok = false
	}
	switch {
	case state == "stopped":
		_, existed := s.m[user]
		delete(s.m, user)
		s.mu.Unlock()
		if existed {
			s.Refresh(ctx, user)
		}
		return
	case !ok && t == nil:
		s.mu.Unlock()
		return // nothing to update and nothing to start from
	case !ok || (t != nil && t.ID != cur.Track.ID):
		cur = State{User: user, Track: *t, StartedAt: startedAt}
	}
	cur.State = state
	if pos != nil {
		cur.PositionMs = *pos
	}
	if playerName != "" {
		cur.PlayerName = playerName
	}
	if playerID != "" {
		cur.PlayerID = playerID
	}
	cur.UpdatedAt = now
	s.m[user] = cur
	s.mu.Unlock()
	s.Refresh(ctx, user)
}

// Drop forgets user's live state (disabled or excluded user) and announces
// live_end to everyone who may have seen it.
func (s *Service) Drop(ctx context.Context, user string, extra ...string) {
	s.mu.Lock()
	_, had := s.m[user]
	delete(s.m, user)
	s.mu.Unlock()
	if had {
		s.Refresh(ctx, user, extra...)
	}
}

// Get returns the user's live state if any.
func (s *Service) Get(user string) (State, bool) {
	now := s.Now().UnixMilli()
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.m[user]
	if ok && st.expired(now) {
		return State{}, false
	}
	return st, ok
}

// Snapshot returns all non-expired states, newest first.
func (s *Service) Snapshot() []State {
	now := s.Now().UnixMilli()
	s.mu.Lock()
	out := make([]State, 0, len(s.m))
	for _, st := range s.m {
		if !st.expired(now) {
			out = append(out, st)
		}
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt != out[j].UpdatedAt {
			return out[i].UpdatedAt > out[j].UpdatedAt
		}
		return out[i].User < out[j].User
	})
	return out
}

// Sweep removes expired states and announces live_end for them.
func (s *Service) Sweep(ctx context.Context) {
	now := s.Now().UnixMilli()
	var gone []string
	s.mu.Lock()
	for u, st := range s.m {
		if st.expired(now) {
			delete(s.m, u)
			gone = append(gone, u)
		}
	}
	s.mu.Unlock()
	for _, u := range gone {
		s.Refresh(ctx, u)
	}
}

// Run sweeps every interval until ctx is done.
func (s *Service) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Sweep(ctx)
		}
	}
}

// Render converts a state to the wire type.
func Render(st State, u model.User) model.Live {
	return model.Live{User: u, Track: st.Track, State: st.State, PositionMs: st.PositionMs,
		StartedAt: model.TS(st.StartedAt), UpdatedAt: model.TS(st.UpdatedAt), PlayerName: st.PlayerName}
}

// Refresh re-announces user's live state to everyone according to the
// current visibility: `live` to those allowed, `live_end` to the others who
// share a circle with them (and to extra, e.g. people who just lost access).
func (s *Service) Refresh(ctx context.Context, user string, extra ...string) {
	// Anyone sharing any circle may have seen the state before a change.
	notify, err := s.Vis.Mates(ctx, user)
	if err != nil {
		s.Log.Error("live audience", "user", user, "err", err)
		return
	}
	for _, u := range extra {
		notify[u] = true
	}
	allowed := map[string]bool{}
	st, ok := s.Get(user)
	if ok {
		allowed, err = s.Vis.LiveAudience(ctx, user, st.Track)
		if err != nil {
			s.Log.Error("live audience", "user", user, "err", err)
			return
		}
		u, err := s.Users.Get(ctx, user)
		if err != nil {
			s.Log.Error("live user", "user", user, "err", err)
			return
		}
		s.Pub.Publish(events.Event{Type: "live", Allow: events.Set(allowed), Data: events.Same(Render(st, u))})
	}
	s.Pub.Publish(events.Event{Type: "live_end",
		Allow: func(v string) bool { return notify[v] && !allowed[v] },
		Data:  events.Same(map[string]string{"username": user})})
}

// VisibleTo lists the live states viewer may see, including their own.
func (s *Service) VisibleTo(ctx context.Context, viewer string) ([]model.Live, error) {
	snap := s.Snapshot()
	var keep []State
	var names []string
	for _, st := range snap {
		ok, err := s.Vis.CanSeeLive(ctx, viewer, st.User, st.Track)
		if err != nil {
			return nil, err
		}
		if ok {
			keep = append(keep, st)
			names = append(names, st.User)
		}
	}
	us, err := s.Users.GetMany(ctx, names)
	if err != nil {
		return nil, err
	}
	out := make([]model.Live, 0, len(keep))
	for _, st := range keep {
		if u, ok := us[st.User]; ok {
			out = append(out, Render(st, u))
		}
	}
	return out, nil
}

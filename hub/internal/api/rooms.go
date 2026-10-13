package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/meliani/cercle/hub/internal/events"
	"github.com/meliani/cercle/hub/internal/model"
	"github.com/meliani/cercle/hub/internal/rooms"
)

func (s *Server) renderRoom(ctx context.Context, rm rooms.Room) (model.Room, error) {
	names := []string{rm.Host}
	for _, l := range rm.Listeners {
		names = append(names, l.User)
	}
	us, err := s.Users.GetMany(ctx, names)
	if err != nil {
		return model.Room{}, err
	}
	get := func(u string) model.User {
		if x, ok := us[u]; ok {
			return x
		}
		return model.User{Username: u, DisplayName: u}
	}
	out := model.Room{ID: rm.ID, CircleID: rm.CircleID, Name: rm.Name, Host: get(rm.Host),
		Listeners: []model.User{}, State: rm.State.Wire(), CreatedAt: model.TS(rm.CreatedAt)}
	for _, l := range rm.Listeners {
		out.Listeners = append(out.Listeners, get(l.User))
	}
	return out, nil
}

func (s *Server) renderRequest(ctx context.Context, q rooms.Request) model.RoomRequest {
	u, err := s.Users.Get(ctx, q.User)
	if err != nil {
		u = model.User{Username: q.User, DisplayName: q.User}
	}
	return model.RoomRequest{ID: q.ID, User: u, Track: q.Track, CreatedAt: model.TS(q.CreatedAt)}
}

// announceRoom sends room_update to every member of the room's circle.
func (s *Server) announceRoom(ctx context.Context, rm rooms.Room) {
	out, err := s.renderRoom(ctx, rm)
	if err != nil {
		s.Log.Warn("render room", "room", rm.ID, "err", err)
		return
	}
	members, err := s.members(ctx, rm.CircleID)
	if err != nil {
		return
	}
	s.Hub.Publish(events.Event{Type: "room_update", Allow: events.Set(setOf(members)), Data: events.Same(out)})
}

func (s *Server) announceClosed(ctx context.Context, rm rooms.Room) {
	aud := map[string]bool{}
	if members, err := s.members(ctx, rm.CircleID); err == nil {
		aud = setOf(members)
	}
	for _, l := range rm.Listeners {
		aud[l.User] = true
	}
	s.Hub.Publish(events.Event{Type: "room_closed", Allow: events.Set(aud), Data: events.Same(map[string]string{"room_id": rm.ID})})
}

// listenersOnly delivers room_state/room_request to subscribed listeners.
func listenersOnly(rm rooms.Room) func(string) bool {
	return func(u string) bool { return rm.IsListener(u) }
}

func (s *Server) leaveRoomAndAnnounce(ctx context.Context, roomID, u string) {
	rm, closed, err := s.Rooms.Leave(ctx, roomID, u)
	if err != nil {
		s.Log.Warn("leave room", "room", roomID, "err", err)
		return
	}
	s.Hub.Unsubscribe(roomID, u)
	if closed {
		s.announceClosed(ctx, rm)
	} else {
		s.announceRoom(ctx, rm)
	}
}

// roomForMember loads a room whose circle me belongs to.
func (s *Server) roomForMember(ctx context.Context, me, id string) (rooms.Room, model.Circle, error) {
	rm, ok := s.Rooms.Get(id)
	if !ok {
		return rm, model.Circle{}, model.ErrNotFound
	}
	c, err := s.circle(ctx, me, rm.CircleID)
	if err != nil {
		return rm, c, err
	}
	if c.MyRole == nil {
		return rm, c, model.ErrNotFound
	}
	return rm, c, nil
}

func (s *Server) writeRoom(w http.ResponseWriter, r *http.Request, rm rooms.Room, status int) {
	out, err := s.renderRoom(r.Context(), rm)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, status, out)
}

func (s *Server) listRooms(w http.ResponseWriter, r *http.Request, me string) {
	c, err := s.memberCircle(r.Context(), me, r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []model.Room{}
	for _, rm := range s.Rooms.ByCircle(c.ID) {
		x, err := s.renderRoom(r.Context(), rm)
		if err != nil {
			s.fail(w, err)
			return
		}
		out = append(out, x)
	}
	writeJSON(w, http.StatusOK, model.List[model.Room]{Items: out})
}

func (s *Server) createRoom(w http.ResponseWriter, r *http.Request, me string) {
	var req struct {
		Name string `json:"name"`
	}
	if !s.decode(w, r, &req) {
		return
	}
	name, ok := validName(req.Name, 100)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_request", "name must be 1..100 characters")
		return
	}
	c, err := s.memberCircle(r.Context(), me, r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	rm, err := s.Rooms.Create(r.Context(), c.ID, name, me)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.announceRoom(r.Context(), rm)
	s.writeRoom(w, r, rm, http.StatusCreated)
}

func (s *Server) getRoom(w http.ResponseWriter, r *http.Request, me string) {
	rm, _, err := s.roomForMember(r.Context(), me, r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	base, err := s.renderRoom(r.Context(), rm)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := model.RoomDetail{Room: base, Requests: []model.RoomRequest{}}
	for _, q := range rm.Requests {
		out.Requests = append(out.Requests, s.renderRequest(r.Context(), q))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) joinRoom(w http.ResponseWriter, r *http.Request, me string) {
	rm, _, err := s.roomForMember(r.Context(), me, r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	was := rm.IsListener(me)
	rm, err = s.Rooms.Join(r.Context(), rm.ID, me)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !was {
		s.announceRoom(r.Context(), rm)
	}
	s.writeRoom(w, r, rm, http.StatusOK)
}

func (s *Server) leaveRoom(w http.ResponseWriter, r *http.Request, me string) {
	rm, ok := s.Rooms.Get(r.PathValue("id"))
	if !ok {
		s.fail(w, model.ErrNotFound)
		return
	}
	if rm.IsListener(me) {
		s.leaveRoomAndAnnounce(r.Context(), rm.ID, me)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) putRoomState(w http.ResponseWriter, r *http.Request, me string) {
	var req struct {
		TrackID    *string  `json:"track_id"`
		PositionMs int64    `json:"position_ms"`
		Playing    bool     `json:"playing"`
		Queue      []string `json:"queue"`
	}
	if !s.decode(w, r, &req) {
		return
	}
	if len(req.Queue) > 1000 {
		writeError(w, http.StatusBadRequest, "bad_request", "queue too long (max 1000)")
		return
	}
	if req.TrackID != nil && strings.TrimSpace(*req.TrackID) == "" {
		req.TrackID = nil
	}
	rm, _, err := s.roomForMember(r.Context(), me, r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	rm, err = s.Rooms.SetState(r.Context(), rm.ID, me, rooms.State{TrackID: req.TrackID, PositionMs: req.PositionMs,
		Playing: req.Playing, Queue: req.Queue})
	if err != nil {
		if err == model.ErrForbidden {
			writeError(w, http.StatusForbidden, "forbidden", "host only")
			return
		}
		s.fail(w, err)
		return
	}
	st := rm.State.Wire()
	s.Hub.Publish(events.Event{Type: "room_state", Room: rm.ID, Allow: listenersOnly(rm),
		Data: events.Same(map[string]any{"room_id": rm.ID, "state": st})})
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) putRoomHost(w http.ResponseWriter, r *http.Request, me string) {
	var req struct {
		Username string `json:"username"`
	}
	if !s.decode(w, r, &req) {
		return
	}
	rm, _, err := s.roomForMember(r.Context(), me, r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	rm, err = s.Rooms.SetHost(r.Context(), rm.ID, me, req.Username)
	switch err {
	case nil:
	case model.ErrForbidden:
		writeError(w, http.StatusForbidden, "forbidden", "host only")
		return
	case model.ErrInvalid:
		writeError(w, http.StatusBadRequest, "bad_request", "the new host must be a listener of the room")
		return
	default:
		s.fail(w, err)
		return
	}
	s.announceRoom(r.Context(), rm)
	s.writeRoom(w, r, rm, http.StatusOK)
}

func (s *Server) addRoomRequest(w http.ResponseWriter, r *http.Request, me string) {
	var req struct {
		Track model.Track `json:"track"`
	}
	if !s.decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Track.ID) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "track.id is required")
		return
	}
	rm, _, err := s.roomForMember(r.Context(), me, r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	rm, q, err := s.Rooms.AddRequest(r.Context(), rm.ID, me, req.Track)
	switch err {
	case nil:
	case model.ErrForbidden:
		writeError(w, http.StatusForbidden, "forbidden", "join the room first")
		return
	case model.ErrConflict:
		writeError(w, http.StatusConflict, "conflict", "too many pending requests")
		return
	default:
		s.fail(w, err)
		return
	}
	out := s.renderRequest(r.Context(), q)
	s.Hub.Publish(events.Event{Type: "room_request", Room: rm.ID, Allow: listenersOnly(rm),
		Data: events.Same(map[string]any{"room_id": rm.ID, "request": out})})
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) deleteRoomRequest(w http.ResponseWriter, r *http.Request, me string) {
	rm, _, err := s.roomForMember(r.Context(), me, r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	rid := r.PathValue("rid")
	rm, err = s.Rooms.DeleteRequest(r.Context(), rm.ID, me, rid)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.Hub.Publish(events.Event{Type: "room_request_deleted", Room: rm.ID, Allow: listenersOnly(rm),
		Data: events.Same(map[string]any{"room_id": rm.ID, "request_id": rid})})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteRoom(w http.ResponseWriter, r *http.Request, me string) {
	rm, c, err := s.roomForMember(r.Context(), me, r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if rm.Host != me && !isManager(c) {
		writeError(w, http.StatusForbidden, "forbidden", "host or circle owner/admin only")
		return
	}
	rm, err = s.Rooms.Close(r.Context(), rm.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.announceClosed(r.Context(), rm)
	w.WriteHeader(http.StatusNoContent)
}

package api

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/meliani/cercle/hub/internal/events"
	"github.com/meliani/cercle/hub/internal/model"
	"github.com/meliani/cercle/hub/internal/users"
)

const everyone = "everyone"

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func encodePlayCursor(at, id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(itoa(at) + "|" + itoa(id)))
}

func decodePlayCursor(c string) (int64, int64, error) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return 0, 0, err
	}
	a, i, ok := strings.Cut(string(b), "|")
	if !ok {
		return 0, 0, errors.New("bad cursor")
	}
	at, err1 := strconv.ParseInt(a, 10, 64)
	id, err2 := strconv.ParseInt(i, 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, errors.New("bad cursor")
	}
	return at, id, nil
}

func randomID(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

const circleCols = `c.id, c.name, c.description, c.kind, c.created_at,
	(SELECT COUNT(*) FROM memberships m WHERE m.circle_id = c.id),
	(SELECT m.role FROM memberships m WHERE m.circle_id = c.id AND m.username = ?)`

func scanCircle(sc users.Scanner) (model.Circle, error) {
	var c model.Circle
	var created int64
	var role sql.NullString
	if err := sc.Scan(&c.ID, &c.Name, &c.Description, &c.Kind, &created, &c.MemberCount, &role); err != nil {
		return c, err
	}
	c.CreatedAt = model.TS(created)
	if role.Valid {
		r := role.String
		c.MyRole = &r
	}
	return c, nil
}

// circlesWhere lists circles matching cond (as seen by me).
func (s *Server) circlesWhere(r *http.Request, me, cond string, args ...any) ([]model.Circle, error) {
	rows, err := s.DB.R.QueryContext(r.Context(), `SELECT `+circleCols+` FROM circles c WHERE `+cond+`
		ORDER BY c.kind = 'everyone' DESC, c.created_at, c.id`, append([]any{me}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Circle{}
	for rows.Next() {
		c, err := scanCircle(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Server) circle(ctx context.Context, me, id string) (model.Circle, error) {
	c, err := scanCircle(s.DB.R.QueryRowContext(ctx, `SELECT `+circleCols+` FROM circles c WHERE c.id = ?`, me, id))
	if errors.Is(err, sql.ErrNoRows) {
		return c, model.ErrNotFound
	}
	return c, err
}

// memberCircle loads a circle that me belongs to; strangers get 404 (403
// for `everyone`, whose existence is no secret).
func (s *Server) memberCircle(ctx context.Context, me, id string) (model.Circle, error) {
	c, err := s.circle(ctx, me, id)
	if err != nil {
		return c, err
	}
	if c.MyRole == nil {
		if c.Kind == everyone {
			return c, newErr(http.StatusForbidden, "forbidden", "you left this circle; join it again to see it")
		}
		return c, model.ErrNotFound
	}
	return c, nil
}

func isManager(c model.Circle) bool {
	return c.MyRole != nil && (*c.MyRole == "owner" || *c.MyRole == "admin")
}

func (s *Server) members(ctx context.Context, circleID string) ([]string, error) {
	rows, err := s.DB.R.QueryContext(ctx, `SELECT username FROM memberships WHERE circle_id = ?`, circleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func setOf(list []string) map[string]bool {
	m := make(map[string]bool, len(list))
	for _, u := range list {
		m[u] = true
	}
	return m
}

// afterMembershipChange re-announces live states whose visibility may have
// changed. members is the circle membership *before* the change (union after).
func (s *Server) afterMembershipChange(ctx context.Context, members []string) {
	for _, u := range members {
		if _, ok := s.Live.Get(u); ok {
			s.Live.Refresh(ctx, u, members...)
		}
	}
}

func (s *Server) listCircles(w http.ResponseWriter, r *http.Request, me string) {
	list, err := s.circlesWhere(r, me, `c.id = 'everyone' OR c.id IN (SELECT circle_id FROM memberships WHERE username = ?)`, me)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, model.List[model.Circle]{Items: list})
}

func validName(n string, max int) (string, bool) {
	n = strings.TrimSpace(n)
	c := utf8.RuneCountInString(n)
	return n, c >= 1 && c <= max
}

func (s *Server) createCircle(w http.ResponseWriter, r *http.Request, me string) {
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if !s.decode(w, r, &req) {
		return
	}
	name, ok := validName(req.Name, 100)
	if !ok || utf8.RuneCountInString(req.Description) > 1000 {
		writeError(w, http.StatusBadRequest, "bad_request", "name must be 1..100 characters, description ≤ 1000")
		return
	}
	id := randomID(8)
	now := s.Now().UnixMilli()
	err := s.DB.Tx(r.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(r.Context(), `INSERT INTO circles (id, name, description, kind, created_by, created_at)
			VALUES (?, ?, ?, 'custom', ?, ?)`, id, name, strings.TrimSpace(req.Description), me, now); err != nil {
			return err
		}
		_, err := tx.ExecContext(r.Context(), `INSERT INTO memberships (circle_id, username, role, joined_at) VALUES (?, ?, 'owner', ?)`, id, me, now)
		return err
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	c, err := s.circle(r.Context(), me, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) getCircle(w http.ResponseWriter, r *http.Request, me string) {
	c, err := s.memberCircle(r.Context(), me, r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	rows, err := s.DB.R.QueryContext(r.Context(), `SELECT username, role, joined_at, share_live, share_history, share_stats, share_discoveries
		FROM memberships WHERE circle_id = ? ORDER BY CASE role WHEN 'owner' THEN 0 WHEN 'admin' THEN 1 ELSE 2 END, joined_at, username`, c.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	type mrow struct {
		user, role string
		joined     int64
		sh         model.Sharing
	}
	var list []mrow
	var names []string
	for rows.Next() {
		var m mrow
		if err := rows.Scan(&m.user, &m.role, &m.joined, &m.sh.ShareLive, &m.sh.ShareHistory, &m.sh.ShareStats, &m.sh.ShareDiscoveries); err != nil {
			rows.Close()
			s.fail(w, err)
			return
		}
		list = append(list, m)
		names = append(names, m.user)
	}
	rows.Close()
	us, err := s.Users.GetMany(r.Context(), names)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := model.CircleDetail{Circle: c, Members: []model.Member{}}
	for _, m := range list {
		mem := model.Member{User: us[m.user], Role: m.role, JoinedAt: model.TS(m.joined)}
		if isManager(c) || m.user == me {
			sh := m.sh
			mem.Sharing = &sh
		}
		out.Members = append(out.Members, mem)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) patchCircle(w http.ResponseWriter, r *http.Request, me string) {
	var req struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
	}
	if !s.decode(w, r, &req) {
		return
	}
	c, err := s.circle(r.Context(), me, r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if c.Kind == everyone {
		u, err := s.Users.Get(r.Context(), me)
		if err != nil {
			s.fail(w, err)
			return
		}
		if !u.IsAdmin {
			writeError(w, http.StatusForbidden, "forbidden", "only server admins may rename the everyone circle")
			return
		}
	} else if c.MyRole == nil {
		s.fail(w, model.ErrNotFound)
		return
	} else if !isManager(c) {
		writeError(w, http.StatusForbidden, "forbidden", "owner or admin only")
		return
	}
	if req.Name != nil {
		name, ok := validName(*req.Name, 100)
		if !ok {
			writeError(w, http.StatusBadRequest, "bad_request", "name must be 1..100 characters")
			return
		}
		if _, err := s.DB.W.ExecContext(r.Context(), `UPDATE circles SET name = ? WHERE id = ?`, name, c.ID); err != nil {
			s.fail(w, err)
			return
		}
	}
	if req.Description != nil {
		if utf8.RuneCountInString(*req.Description) > 1000 {
			writeError(w, http.StatusBadRequest, "bad_request", "description must be ≤ 1000 characters")
			return
		}
		if _, err := s.DB.W.ExecContext(r.Context(), `UPDATE circles SET description = ? WHERE id = ?`, strings.TrimSpace(*req.Description), c.ID); err != nil {
			s.fail(w, err)
			return
		}
	}
	c, err = s.circle(r.Context(), me, c.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// destroyCircle deletes a custom circle and everything in it.
func (s *Server) destroyCircle(ctx context.Context, id string) error {
	members, err := s.members(ctx, id)
	if err != nil {
		return err
	}
	if _, err := s.DB.W.ExecContext(ctx, `DELETE FROM circles WHERE id = ? AND kind = 'custom'`, id); err != nil {
		return err
	}
	aud := setOf(members)
	for _, rm := range s.Rooms.Forget(id) {
		s.Hub.Publish(events.Event{Type: "room_closed", Allow: events.Set(aud), Data: events.Same(map[string]string{"room_id": rm.ID})})
	}
	s.afterMembershipChange(ctx, members)
	return nil
}

func (s *Server) deleteCircle(w http.ResponseWriter, r *http.Request, me string) {
	c, err := s.memberCircle(r.Context(), me, r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if c.Kind == everyone {
		writeError(w, http.StatusForbidden, "forbidden", "the everyone circle cannot be deleted")
		return
	}
	if *c.MyRole != "owner" {
		writeError(w, http.StatusForbidden, "forbidden", "owner only")
		return
	}
	if err := s.destroyCircle(r.Context(), c.ID); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

var (
	errInviteInvalid = newErr(http.StatusGone, "invite_invalid", "this invite has expired or reached its maximum uses")
	errCodeRequired  = newErr(http.StatusBadRequest, "code_required", "an invite code is required to join this circle")
)

// redeem joins me to a circle with an invite code (expectCircle optional).
func (s *Server) redeem(ctx context.Context, me, code, expectCircle string) (string, error) {
	var circleID string
	err := s.DB.Tx(ctx, func(tx *sql.Tx) error {
		var exp int64
		var maxUses, uses int
		err := tx.QueryRowContext(ctx, `SELECT circle_id, expires_at, max_uses, uses FROM invites WHERE code = ?`, code).
			Scan(&circleID, &exp, &maxUses, &uses)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && expectCircle != "" && circleID != expectCircle) {
			return newErr(http.StatusNotFound, "not_found", "unknown invite code")
		}
		if err != nil {
			return err
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM memberships WHERE circle_id = ? AND username = ?`, circleID, me).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return nil // already a member: idempotent, no use consumed
		}
		if exp <= s.Now().UnixMilli() || (maxUses > 0 && uses >= maxUses) {
			return errInviteInvalid
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO memberships (circle_id, username, role, joined_at) VALUES (?, ?, 'member', ?)`,
			circleID, me, s.Now().UnixMilli()); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE invites SET uses = uses + 1 WHERE code = ?`, code)
		return err
	})
	return circleID, err
}

func (s *Server) joinCircle(w http.ResponseWriter, r *http.Request, me string) {
	var req struct {
		Code string `json:"code"`
	}
	if !s.decode(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	c, err := s.circle(r.Context(), me, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	if c.Kind == everyone {
		if _, err := s.DB.W.ExecContext(r.Context(), `INSERT OR IGNORE INTO memberships (circle_id, username, role, joined_at)
			VALUES ('everyone', ?, 'member', ?)`, me, s.Now().UnixMilli()); err != nil {
			s.fail(w, err)
			return
		}
	} else if c.MyRole == nil {
		if req.Code == "" {
			s.fail(w, errCodeRequired)
			return
		}
		if _, err := s.redeem(r.Context(), me, req.Code, id); err != nil {
			s.fail(w, err)
			return
		}
	}
	s.finishJoin(w, r, me, id)
}

func (s *Server) finishJoin(w http.ResponseWriter, r *http.Request, me, id string) {
	if members, err := s.members(r.Context(), id); err == nil {
		s.afterMembershipChange(r.Context(), members)
	}
	c, err := s.circle(r.Context(), me, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) acceptInvite(w http.ResponseWriter, r *http.Request, me string) {
	id, err := s.redeem(r.Context(), me, r.PathValue("code"), "")
	if err != nil {
		s.fail(w, err)
		return
	}
	s.finishJoin(w, r, me, id)
}

// removeMember drops u from a circle: rooms first, then the membership, then
// live re-announcement.
func (s *Server) removeMember(ctx context.Context, circleID, u string) error {
	members, err := s.members(ctx, circleID)
	if err != nil {
		return err
	}
	for _, roomID := range s.Rooms.RoomsWith(circleID, u) {
		s.leaveRoomAndAnnounce(ctx, roomID, u)
	}
	if _, err := s.DB.W.ExecContext(ctx, `DELETE FROM memberships WHERE circle_id = ? AND username = ?`, circleID, u); err != nil {
		return err
	}
	s.afterMembershipChange(ctx, members)
	return nil
}

func (s *Server) leaveCircle(w http.ResponseWriter, r *http.Request, me string) {
	c, err := s.circle(r.Context(), me, r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if c.MyRole == nil {
		w.WriteHeader(http.StatusNoContent) // already out: idempotent
		return
	}
	if *c.MyRole == "owner" {
		if c.MemberCount <= 1 {
			if err := s.destroyCircle(r.Context(), c.ID); err != nil {
				s.fail(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeError(w, http.StatusConflict, "owner_must_transfer", "transfer ownership before leaving")
		return
	}
	if err := s.removeMember(r.Context(), c.ID, me); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) putSharing(w http.ResponseWriter, r *http.Request, me string) {
	var req struct {
		ShareLive        *bool `json:"share_live"`
		ShareHistory     *bool `json:"share_history"`
		ShareStats       *bool `json:"share_stats"`
		ShareDiscoveries *bool `json:"share_discoveries"`
	}
	if !s.decode(w, r, &req) {
		return
	}
	c, err := s.memberCircle(r.Context(), me, r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	set := func(col string, v *bool) error {
		if v == nil {
			return nil
		}
		_, err := s.DB.W.ExecContext(r.Context(), `UPDATE memberships SET `+col+` = ? WHERE circle_id = ? AND username = ?`,
			*v, c.ID, me)
		return err
	}
	for _, f := range []struct {
		col string
		v   *bool
	}{{"share_live", req.ShareLive}, {"share_history", req.ShareHistory}, {"share_stats", req.ShareStats}, {"share_discoveries", req.ShareDiscoveries}} {
		if err := set(f.col, f.v); err != nil {
			s.fail(w, err)
			return
		}
	}
	var sh model.Sharing
	if err := s.DB.R.QueryRowContext(r.Context(), `SELECT share_live, share_history, share_stats, share_discoveries
		FROM memberships WHERE circle_id = ? AND username = ?`, c.ID, me).
		Scan(&sh.ShareLive, &sh.ShareHistory, &sh.ShareStats, &sh.ShareDiscoveries); err != nil {
		s.fail(w, err)
		return
	}
	if req.ShareLive != nil {
		if members, err := s.members(r.Context(), c.ID); err == nil {
			s.Live.Refresh(r.Context(), me, members...)
		}
	}
	writeJSON(w, http.StatusOK, sh)
}

func (s *Server) memberRole(ctx context.Context, circleID, u string) (string, error) {
	var role string
	err := s.DB.R.QueryRowContext(ctx, `SELECT role FROM memberships WHERE circle_id = ? AND username = ?`, circleID, u).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", newErr(http.StatusNotFound, "not_found", u+" is not a member of this circle")
	}
	return role, err
}

func (s *Server) putMember(w http.ResponseWriter, r *http.Request, me string) {
	var req struct {
		Role string `json:"role"`
	}
	if !s.decode(w, r, &req) {
		return
	}
	c, err := s.memberCircle(r.Context(), me, r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	target := r.PathValue("username")
	if c.Kind == everyone {
		writeError(w, http.StatusForbidden, "forbidden", "the everyone circle has no roles")
		return
	}
	if *c.MyRole != "owner" {
		writeError(w, http.StatusForbidden, "forbidden", "owner only")
		return
	}
	cur, err := s.memberRole(r.Context(), c.ID, target)
	if err != nil {
		s.fail(w, err)
		return
	}
	switch req.Role {
	case "admin", "member":
		if cur == "owner" {
			writeError(w, http.StatusBadRequest, "bad_request", "transfer ownership (role \"owner\") to someone else first")
			return
		}
		if _, err := s.DB.W.ExecContext(r.Context(), `UPDATE memberships SET role = ? WHERE circle_id = ? AND username = ?`, req.Role, c.ID, target); err != nil {
			s.fail(w, err)
			return
		}
	case "owner":
		if target != me {
			err := s.DB.Tx(r.Context(), func(tx *sql.Tx) error {
				if _, err := tx.ExecContext(r.Context(), `UPDATE memberships SET role = 'admin' WHERE circle_id = ? AND username = ?`, c.ID, me); err != nil {
					return err
				}
				_, err := tx.ExecContext(r.Context(), `UPDATE memberships SET role = 'owner' WHERE circle_id = ? AND username = ?`, c.ID, target)
				return err
			})
			if err != nil {
				s.fail(w, err)
				return
			}
		}
	default:
		writeError(w, http.StatusBadRequest, "bad_request", "role must be owner, admin or member")
		return
	}
	var m model.Member
	var joined int64
	var sh model.Sharing
	if err := s.DB.R.QueryRowContext(r.Context(), `SELECT role, joined_at, share_live, share_history, share_stats, share_discoveries
		FROM memberships WHERE circle_id = ? AND username = ?`, c.ID, target).
		Scan(&m.Role, &joined, &sh.ShareLive, &sh.ShareHistory, &sh.ShareStats, &sh.ShareDiscoveries); err != nil {
		s.fail(w, err)
		return
	}
	m.JoinedAt = model.TS(joined)
	m.Sharing = &sh
	if m.User, err = s.Users.Get(r.Context(), target); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) deleteMember(w http.ResponseWriter, r *http.Request, me string) {
	c, err := s.memberCircle(r.Context(), me, r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	target := r.PathValue("username")
	if c.Kind == everyone {
		writeError(w, http.StatusForbidden, "forbidden", "nobody can be removed from the everyone circle")
		return
	}
	if !isManager(c) {
		writeError(w, http.StatusForbidden, "forbidden", "owner or admin only")
		return
	}
	if target == me {
		writeError(w, http.StatusBadRequest, "bad_request", "use POST /circles/{id}/leave to leave")
		return
	}
	role, err := s.memberRole(r.Context(), c.ID, target)
	if err != nil {
		s.fail(w, err)
		return
	}
	if role == "owner" || (role == "admin" && *c.MyRole != "owner") {
		writeError(w, http.StatusForbidden, "forbidden", "you cannot remove this member")
		return
	}
	if err := s.removeMember(r.Context(), c.ID, target); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- invites ----------------------------------------------------------

func renderInvite(code, circle, by string, exp int64, maxUses, uses int) model.Invite {
	return model.Invite{Code: code, CircleID: circle, CreatedBy: by, ExpiresAt: model.TS(exp),
		MaxUses: maxUses, Uses: uses, URLPath: "/cercle/join/" + code}
}

func (s *Server) managedCircle(w http.ResponseWriter, r *http.Request, me string) (model.Circle, bool) {
	c, err := s.memberCircle(r.Context(), me, r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return c, false
	}
	if c.Kind == everyone {
		writeError(w, http.StatusBadRequest, "bad_request", "the everyone circle needs no invites")
		return c, false
	}
	if !isManager(c) {
		writeError(w, http.StatusForbidden, "forbidden", "owner or admin only")
		return c, false
	}
	return c, true
}

func (s *Server) createInvite(w http.ResponseWriter, r *http.Request, me string) {
	req := struct {
		ExpiresInHours *int `json:"expires_in_hours"`
		MaxUses        *int `json:"max_uses"`
	}{}
	if !s.decode(w, r, &req) {
		return
	}
	c, ok := s.managedCircle(w, r, me)
	if !ok {
		return
	}
	hours, maxUses := 168, 0
	if req.ExpiresInHours != nil {
		hours = *req.ExpiresInHours
	}
	if req.MaxUses != nil {
		maxUses = *req.MaxUses
	}
	if hours < 1 || hours > 24*365 || maxUses < 0 || maxUses > 100000 {
		writeError(w, http.StatusBadRequest, "bad_request", "expires_in_hours must be 1..8760 and max_uses ≥ 0")
		return
	}
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	code := base64.RawURLEncoding.EncodeToString(b)
	now := s.Now()
	exp := now.Add(time.Duration(hours) * time.Hour).UnixMilli()
	if _, err := s.DB.W.ExecContext(r.Context(), `INSERT INTO invites (code, circle_id, created_by, created_at, expires_at, max_uses, uses)
		VALUES (?, ?, ?, ?, ?, ?, 0)`, code, c.ID, me, now.UnixMilli(), exp, maxUses); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, renderInvite(code, c.ID, me, exp, maxUses, 0))
}

func (s *Server) listInvites(w http.ResponseWriter, r *http.Request, me string) {
	c, ok := s.managedCircle(w, r, me)
	if !ok {
		return
	}
	rows, err := s.DB.R.QueryContext(r.Context(), `SELECT code, created_by, expires_at, max_uses, uses FROM invites
		WHERE circle_id = ? ORDER BY created_at DESC`, c.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer rows.Close()
	out := []model.Invite{}
	for rows.Next() {
		var code, by string
		var exp int64
		var mu, u int
		if err := rows.Scan(&code, &by, &exp, &mu, &u); err != nil {
			s.fail(w, err)
			return
		}
		out = append(out, renderInvite(code, c.ID, by, exp, mu, u))
	}
	writeJSON(w, http.StatusOK, model.List[model.Invite]{Items: out})
}

func (s *Server) deleteInvite(w http.ResponseWriter, r *http.Request, me string) {
	c, ok := s.managedCircle(w, r, me)
	if !ok {
		return
	}
	res, err := s.DB.W.ExecContext(r.Context(), `DELETE FROM invites WHERE code = ? AND circle_id = ?`, r.PathValue("code"), c.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		s.fail(w, model.ErrNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getInvite(w http.ResponseWriter, r *http.Request, me string) {
	var circleID, name, by string
	var exp int64
	var maxUses, uses, count int
	err := s.DB.R.QueryRowContext(r.Context(), `SELECT i.circle_id, c.name, i.created_by, i.expires_at, i.max_uses, i.uses,
		(SELECT COUNT(*) FROM memberships m WHERE m.circle_id = c.id)
		FROM invites i JOIN circles c ON c.id = i.circle_id WHERE i.code = ?`, r.PathValue("code")).
		Scan(&circleID, &name, &by, &exp, &maxUses, &uses, &count)
	if err != nil {
		s.fail(w, err)
		return
	}
	creator, err := s.Users.Get(r.Context(), by)
	if err != nil {
		creator = model.User{Username: by, DisplayName: by}
	}
	valid := exp > s.Now().UnixMilli() && (maxUses == 0 || uses < maxUses)
	writeJSON(w, http.StatusOK, map[string]any{
		"circle":     map[string]any{"id": circleID, "name": name, "member_count": count},
		"created_by": creator,
		"valid":      valid,
	})
}

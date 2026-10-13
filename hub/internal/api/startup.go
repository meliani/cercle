package api

import (
	"context"
	"database/sql"
	"errors"
)

// applyEveryoneNaming writes CERCLE_EVERYONE_NAME / _DESCRIPTION to the
// `everyone` circle. Unset variables leave the row alone, so a rename done
// through the API by a server admin survives restarts.
func (s *Server) applyEveryoneNaming(ctx context.Context) error {
	if s.Cfg.EveryoneName != "" {
		if _, err := s.DB.W.ExecContext(ctx, `UPDATE circles SET name = ? WHERE id = 'everyone'`, s.Cfg.EveryoneName); err != nil {
			return err
		}
	}
	if s.Cfg.EveryoneDescription != "" {
		if _, err := s.DB.W.ExecContext(ctx, `UPDATE circles SET description = ? WHERE id = 'everyone'`, s.Cfg.EveryoneDescription); err != nil {
			return err
		}
	}
	return nil
}

// purgeExcluded removes users that are now listed in CERCLE_EXCLUDED_USERS
// from every circle (handing ownership on, or deleting a circle they were
// alone in), revokes their sessions and drops their live state. Their plays
// are kept but, with no circle left, nobody else can see them.
func (s *Server) purgeExcluded(ctx context.Context) error {
	if len(s.Cfg.ExcludedUsers) == 0 {
		return nil
	}
	rows, err := s.DB.R.QueryContext(ctx, `SELECT username FROM users`)
	if err != nil {
		return err
	}
	var excluded []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			rows.Close()
			return err
		}
		if s.Users.IsExcluded(u) {
			excluded = append(excluded, u)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, u := range excluded {
		type mem struct{ circle, role, kind string }
		var mems []mem
		mr, err := s.DB.R.QueryContext(ctx, `SELECT m.circle_id, m.role, c.kind FROM memberships m
			JOIN circles c ON c.id = m.circle_id WHERE m.username = ?`, u)
		if err != nil {
			return err
		}
		for mr.Next() {
			var m mem
			if err := mr.Scan(&m.circle, &m.role, &m.kind); err != nil {
				mr.Close()
				return err
			}
			mems = append(mems, m)
		}
		mr.Close()
		for _, m := range mems {
			if m.role == "owner" && m.kind == "custom" {
				var next string
				err := s.DB.R.QueryRowContext(ctx, `SELECT username FROM memberships WHERE circle_id = ? AND username <> ?
					ORDER BY CASE role WHEN 'admin' THEN 0 ELSE 1 END, joined_at, username LIMIT 1`, m.circle, u).Scan(&next)
				if errors.Is(err, sql.ErrNoRows) {
					if err := s.destroyCircle(ctx, m.circle); err != nil {
						return err
					}
					continue
				}
				if err != nil {
					return err
				}
				if _, err := s.DB.W.ExecContext(ctx, `UPDATE memberships SET role = 'owner' WHERE circle_id = ? AND username = ?`, m.circle, next); err != nil {
					return err
				}
			}
			if err := s.removeMember(ctx, m.circle, u); err != nil {
				return err
			}
		}
		if _, err := s.DB.W.ExecContext(ctx, `DELETE FROM sessions WHERE username = ?`, u); err != nil {
			return err
		}
		s.Live.Drop(ctx, u)
		if len(mems) > 0 {
			s.Log.Info("excluded user removed from circles", "user", u, "circles", len(mems))
		}
	}
	return nil
}

// Package visibility implements the single visibility rule of
// docs/CONTRACT.md §1. Every REST handler and every WebSocket fan-out goes
// through this package; nothing else decides who may see what.
//
//	viewer V may see item X of subject S iff V == S, or
//	  - there is a circle where both V and S are members and S's membership
//	    has the sharing flag for X's category, and
//	  - X is not private (private = 0 and not matching a hidden item), and
//	  - for history: played_at <= now - S.history_delay_minutes, and
//	  - for live: S is not paused.
//
// Membership is evaluated at query time, so leaving a circle immediately
// removes visibility through it, including the past.
package visibility

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/meliani/cercle/hub/internal/db"
	"github.com/meliani/cercle/hub/internal/model"
)

// Category is what kind of data is being looked at.
type Category string

const (
	Live        Category = "live"
	History     Category = "history"
	Stats       Category = "stats"
	Discoveries Category = "discoveries"
)

// Column returns the memberships flag column for c.
func (c Category) Column() string {
	switch c {
	case Live:
		return "share_live"
	case History:
		return "share_history"
	case Stats:
		return "share_stats"
	case Discoveries:
		return "share_discoveries"
	}
	panic(fmt.Sprintf("visibility: unknown category %q", c))
}

// V evaluates the rule against the read pool.
type V struct {
	Q   db.Querier
	Now func() time.Time
}

func New(q db.Querier, now func() time.Time) *V { return &V{Q: q, Now: now} }

func (v *V) now() int64 { return v.Now().UnixMilli() }

// SharesWith reports whether subject shares category c with viewer through
// at least one common circle (optionally only through circleID). V == S is
// always true.
func (v *V) SharesWith(ctx context.Context, viewer, subject string, c Category, circleID string) (bool, error) {
	if viewer == subject {
		return true, nil
	}
	q := `SELECT 1 FROM memberships a JOIN memberships b ON b.circle_id = a.circle_id
		WHERE a.username = ? AND b.username = ? AND b.` + c.Column() + ` = 1`
	args := []any{viewer, subject}
	if circleID != "" {
		q += ` AND a.circle_id = ?`
		args = append(args, circleID)
	}
	var one int
	err := v.Q.QueryRowContext(ctx, q+` LIMIT 1`, args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// CommonCircle reports whether viewer and subject share any circle at all
// (regardless of flags) — used for "does this user exist for me" checks.
func (v *V) CommonCircle(ctx context.Context, viewer, subject string) (bool, error) {
	if viewer == subject {
		return true, nil
	}
	var one int
	err := v.Q.QueryRowContext(ctx, `SELECT 1 FROM memberships a JOIN memberships b ON b.circle_id = a.circle_id
		WHERE a.username = ? AND b.username = ? LIMIT 1`, viewer, subject).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Audience returns every user who may see subject's category c at the
// membership level, including subject.
func (v *V) Audience(ctx context.Context, subject string, c Category) (map[string]bool, error) {
	out := map[string]bool{subject: true}
	rows, err := v.Q.QueryContext(ctx, `SELECT DISTINCT a.username FROM memberships a
		JOIN memberships b ON b.circle_id = a.circle_id
		WHERE b.username = ? AND b.`+c.Column()+` = 1`, subject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out[u] = true
	}
	return out, rows.Err()
}

// Mates returns every user sharing at least one circle with subject
// (regardless of flags), including subject.
func (v *V) Mates(ctx context.Context, subject string) (map[string]bool, error) {
	out := map[string]bool{subject: true}
	rows, err := v.Q.QueryContext(ctx, `SELECT DISTINCT a.username FROM memberships a
		JOIN memberships b ON b.circle_id = a.circle_id WHERE b.username = ?`, subject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out[u] = true
	}
	return out, rows.Err()
}

// Subjects returns every user whose category c viewer may see (membership
// level), including viewer. If circleID is set, only through that circle.
func (v *V) Subjects(ctx context.Context, viewer string, c Category, circleID string) ([]string, error) {
	q := `SELECT DISTINCT b.username FROM memberships a JOIN memberships b ON b.circle_id = a.circle_id
		WHERE a.username = ? AND b.` + c.Column() + ` = 1`
	args := []any{viewer}
	if circleID != "" {
		q += ` AND a.circle_id = ?`
		args = append(args, circleID)
	}
	rows, err := v.Q.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{viewer}
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		if u != viewer {
			out = append(out, u)
		}
	}
	return out, rows.Err()
}

// hiddenMatch is the SQL predicate "play alias p matches one of its owner's
// hidden items".
func hiddenMatch(p string) string {
	return `EXISTS (SELECT 1 FROM hidden_items h WHERE h.username = ` + p + `.user_id AND (
		(h.kind = 'track' AND h.item_id = ` + p + `.track_id) OR
		(h.kind = 'album' AND h.item_id = ` + p + `.album_id) OR
		(h.kind = 'artist' AND h.item_id = ` + p + `.artist_id)))`
}

// PublicPlayCond is the item-level half of the rule for plays (history and
// stats) seen by someone other than their owner: not private, not hidden,
// older than the owner's history delay. Use with alias p of table plays.
func (v *V) PublicPlayCond(p string) (string, []any) {
	return `(` + p + `.private = 0 AND NOT ` + hiddenMatch(p) + ` AND ` + p + `.played_at <= ? -
		60000 * (SELECT u.history_delay_minutes FROM users u WHERE u.username = ` + p + `.user_id))`, []any{v.now()}
}

// FeedCond returns the full rule for feed_items alias f as seen by the
// viewer expression viewerSQL (either "?" with viewerArgs, or a column such
// as "vu.username" with no args). circleID optionally restricts the circle
// that grants visibility. The plays alias "fp" is reserved.
func (v *V) FeedCond(viewerSQL string, viewerArgs []any, circleID string) (string, []any) {
	var args []any
	args = append(args, viewerArgs...) // f.user_id = V
	args = append(args, v.now())       // delay
	args = append(args, viewerArgs...) // membership
	circleSQL := ""
	if circleID != "" {
		circleSQL = ` AND a.circle_id = ?`
		args = append(args, circleID)
	}
	cond := `(f.user_id = ` + viewerSQL + ` OR (
		f.private = 0
		AND f.last_played_at <= ? - 60000 * (SELECT u.history_delay_minutes FROM users u WHERE u.username = f.user_id)
		AND EXISTS (SELECT 1 FROM memberships a JOIN memberships b ON b.circle_id = a.circle_id
			WHERE a.username = ` + viewerSQL + ` AND b.username = f.user_id` + circleSQL + `
			AND ((f.kind = 'listened' AND b.share_history = 1) OR (f.kind <> 'listened' AND b.share_discoveries = 1)))
		AND NOT EXISTS (SELECT 1 FROM plays fp WHERE (fp.feed_item_id = f.id OR fp.id = f.last_play_id) AND ` + hiddenMatch("fp") + `)
	))`
	return cond, args
}

// CanSeeFeedItem applies the rule to one feed item.
func (v *V) CanSeeFeedItem(ctx context.Context, viewer, itemID string) (bool, error) {
	cond, args := v.FeedCond("?", []any{viewer}, "")
	var one int
	err := v.Q.QueryRowContext(ctx, `SELECT 1 FROM feed_items f WHERE f.id = ? AND `+cond, append([]any{itemID}, args...)...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// FeedItemAudience returns every user allowed to see the feed item.
func (v *V) FeedItemAudience(ctx context.Context, itemID string) (map[string]bool, error) {
	cond, args := v.FeedCond("vu.username", nil, "")
	rows, err := v.Q.QueryContext(ctx, `SELECT vu.username FROM users vu, feed_items f WHERE f.id = ? AND `+cond,
		append([]any{itemID}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out[u] = true
	}
	return out, rows.Err()
}

// CanModerateFeedItem: viewer is owner/admin of a circle that makes the item
// visible to its members (subject shares the item's category there and the
// item passes the item-level checks).
func (v *V) CanModerateFeedItem(ctx context.Context, viewer, itemID string) (bool, error) {
	cond, args := v.FeedCond("?", []any{viewer}, "")
	var one int
	err := v.Q.QueryRowContext(ctx, `SELECT 1 FROM feed_items f WHERE f.id = ? AND `+cond+`
		AND EXISTS (SELECT 1 FROM memberships a JOIN memberships b ON b.circle_id = a.circle_id
			WHERE a.username = ? AND a.role IN ('owner','admin') AND b.username = f.user_id
			AND ((f.kind = 'listened' AND b.share_history = 1) OR (f.kind <> 'listened' AND b.share_discoveries = 1)))`,
		append(append([]any{itemID}, args...), viewer)...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// LiveGate is the item-level half of the rule for a live track: subject not
// paused and track not matching a hidden item. It does not check membership.
func (v *V) LiveGate(ctx context.Context, subject string, t model.Track) (bool, error) {
	var paused sql.NullInt64
	err := v.Q.QueryRowContext(ctx, `SELECT paused_until FROM users WHERE username = ?`, subject).Scan(&paused)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if paused.Valid && paused.Int64 > v.now() {
		return false, nil
	}
	hidden, err := HiddenMatch(ctx, v.Q, subject, t.ID, t.AlbumID, t.ArtistID)
	return !hidden, err
}

// CanSeeLive applies the rule to a live state.
func (v *V) CanSeeLive(ctx context.Context, viewer, subject string, t model.Track) (bool, error) {
	if viewer == subject {
		return true, nil
	}
	ok, err := v.LiveGate(ctx, subject, t)
	if err != nil || !ok {
		return false, err
	}
	return v.SharesWith(ctx, viewer, subject, Live, "")
}

// LiveAudience returns everyone who may see subject's live track t.
func (v *V) LiveAudience(ctx context.Context, subject string, t model.Track) (map[string]bool, error) {
	ok, err := v.LiveGate(ctx, subject, t)
	if err != nil {
		return nil, err
	}
	if !ok {
		return map[string]bool{subject: true}, nil
	}
	return v.Audience(ctx, subject, Live)
}

// HiddenMatch reports whether any of the ids matches one of user's hidden
// items. Empty ids never match.
func HiddenMatch(ctx context.Context, q db.Querier, user, trackID, albumID, artistID string) (bool, error) {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM hidden_items WHERE username = ? AND (
		(kind = 'track' AND item_id = ? AND ? <> '') OR
		(kind = 'album' AND item_id = ? AND ? <> '') OR
		(kind = 'artist' AND item_id = ? AND ? <> '')) LIMIT 1`,
		user, trackID, trackID, albumID, albumID, artistID, artistID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

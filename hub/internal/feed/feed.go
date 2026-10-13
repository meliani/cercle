// Package feed serves feed items, reactions and comments. Items themselves
// are written at ingest time (package ingest); this package only reads them,
// always through the visibility rule.
package feed

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/meliani/cercle/hub/internal/db"
	"github.com/meliani/cercle/hub/internal/events"
	"github.com/meliani/cercle/hub/internal/model"
	"github.com/meliani/cercle/hub/internal/users"
	"github.com/meliani/cercle/hub/internal/visibility"
)

// Service is the feed read side.
type Service struct {
	DB    *db.DB
	Vis   *visibility.V
	Users *users.Service
	Pub   events.Publisher
	Now   func() time.Time
}

const itemCols = `f.id, f.kind, f.user_id, f.play_count, f.started_at, f.ended_at,
	p.track_id, p.title, p.artist, p.artist_id, p.album, p.album_id, p.album_artist, p.duration_ms`

const itemFrom = ` FROM feed_items f JOIN plays p ON p.id = f.last_play_id `

type itemRow struct {
	id, kind, user string
	count          int
	started, ended int64
	track          model.Track
	albumArtist    string
}

func scanItem(sc users.Scanner) (itemRow, error) {
	var r itemRow
	err := sc.Scan(&r.id, &r.kind, &r.user, &r.count, &r.started, &r.ended,
		&r.track.ID, &r.track.Title, &r.track.Artist, &r.track.ArtistID, &r.track.Album, &r.track.AlbumID,
		&r.albumArtist, &r.track.DurationMs)
	return r, err
}

func (r itemRow) base(u model.User) model.FeedItem {
	it := model.FeedItem{ID: r.id, Kind: r.kind, User: u, PlayCount: r.count,
		StartedAt: model.TS(r.started), EndedAt: model.TS(r.ended), Reactions: []model.Reaction{}}
	t := r.track
	it.Track = &t
	artist := r.albumArtist
	if artist == "" {
		artist = r.track.Artist
	}
	switch r.kind {
	case "listened", "discovery_album":
		it.Album = &model.AlbumRef{ID: r.track.AlbumID, Name: r.track.Album, Artist: artist}
		it.Artist = &model.ArtistRef{ID: r.track.ArtistID, Name: r.track.Artist}
	case "discovery_artist":
		it.Artist = &model.ArtistRef{ID: r.track.ArtistID, Name: r.track.Artist}
	}
	return it
}

// ListOpts filters the feed.
type ListOpts struct {
	Circle string // only visibility granted through this circle (+ my own items)
	User   string // only this subject
	Limit  int
	Before string // cursor
}

func encodeCursor(ended int64, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(ended, 10) + "|" + id))
}

func decodeCursor(c string) (int64, string, error) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return 0, "", model.ErrInvalid
	}
	ms, id, ok := strings.Cut(string(b), "|")
	if !ok {
		return 0, "", model.ErrInvalid
	}
	n, err := strconv.ParseInt(ms, 10, 64)
	if err != nil {
		return 0, "", model.ErrInvalid
	}
	return n, id, nil
}

// List returns the viewer's feed, newest first.
func (s *Service) List(ctx context.Context, viewer string, o ListOpts) (model.Page[model.FeedItem], error) {
	cond, args := s.Vis.FeedCond("?", []any{viewer}, o.Circle)
	q := `SELECT ` + itemCols + itemFrom + ` WHERE ` + cond
	if o.User != "" {
		q += ` AND f.user_id = ?`
		args = append(args, o.User)
	}
	if o.Circle != "" {
		q += ` AND f.user_id IN (SELECT username FROM memberships WHERE circle_id = ?)`
		args = append(args, o.Circle)
	}
	if o.Before != "" {
		ended, id, err := decodeCursor(o.Before)
		if err != nil {
			return model.Page[model.FeedItem]{}, err
		}
		q += ` AND (f.ended_at < ? OR (f.ended_at = ? AND f.id < ?))`
		args = append(args, ended, ended, id)
	}
	q += ` ORDER BY f.ended_at DESC, f.id DESC LIMIT ?`
	args = append(args, o.Limit+1)
	rows, err := s.DB.R.QueryContext(ctx, q, args...)
	if err != nil {
		return model.Page[model.FeedItem]{}, err
	}
	var list []itemRow
	for rows.Next() {
		r, err := scanItem(rows)
		if err != nil {
			rows.Close()
			return model.Page[model.FeedItem]{}, err
		}
		list = append(list, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return model.Page[model.FeedItem]{}, err
	}
	page := model.Page[model.FeedItem]{Items: []model.FeedItem{}}
	if len(list) > o.Limit {
		list = list[:o.Limit]
		last := list[len(list)-1]
		c := encodeCursor(last.ended, last.id)
		page.Next = &c
	}
	items, err := s.render(ctx, viewer, list)
	if err != nil {
		return page, err
	}
	page.Items = items
	return page, nil
}

func (s *Service) render(ctx context.Context, viewer string, list []itemRow) ([]model.FeedItem, error) {
	out := make([]model.FeedItem, 0, len(list))
	if len(list) == 0 {
		return out, nil
	}
	names := make([]string, 0, len(list))
	ids := make([]string, 0, len(list))
	for _, r := range list {
		names = append(names, r.user)
		ids = append(ids, r.id)
	}
	us, err := s.Users.GetMany(ctx, names)
	if err != nil {
		return nil, err
	}
	reacts, err := s.reactions(ctx, ids)
	if err != nil {
		return nil, err
	}
	counts, err := s.commentCounts(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, r := range list {
		it := r.base(us[r.user])
		it.Reactions = reacts[r.id].For(viewer)
		it.CommentCount = counts[r.id]
		out = append(out, it)
	}
	return out, nil
}

// Reactors records who reacted with what, in first-use order.
type Reactors struct {
	order []string
	who   map[string]map[string]bool
}

// For renders the reactions for one viewer.
func (r *Reactors) For(viewer string) []model.Reaction {
	out := []model.Reaction{}
	if r == nil {
		return out
	}
	for _, e := range r.order {
		out = append(out, model.Reaction{Emoji: e, Count: len(r.who[e]), Mine: r.who[e][viewer]})
	}
	return out
}

func (s *Service) reactions(ctx context.Context, ids []string) (map[string]*Reactors, error) {
	out := map[string]*Reactors{}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.DB.R.QueryContext(ctx, `SELECT item_id, emoji, user_id FROM reactions
		WHERE item_id IN (`+db.Placeholders(len(ids))+`) ORDER BY created_at, emoji, user_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var item, emoji, user string
		if err := rows.Scan(&item, &emoji, &user); err != nil {
			return nil, err
		}
		r := out[item]
		if r == nil {
			r = &Reactors{who: map[string]map[string]bool{}}
			out[item] = r
		}
		if r.who[emoji] == nil {
			r.who[emoji] = map[string]bool{}
			r.order = append(r.order, emoji)
		}
		r.who[emoji][user] = true
	}
	return out, rows.Err()
}

func (s *Service) commentCounts(ctx context.Context, ids []string) (map[string]int, error) {
	out := map[string]int{}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.DB.R.QueryContext(ctx, `SELECT item_id, COUNT(*) FROM comments
		WHERE item_id IN (`+db.Placeholders(len(ids))+`) GROUP BY item_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

func (s *Service) loadRow(ctx context.Context, id string) (itemRow, error) {
	r, err := scanItem(s.DB.R.QueryRowContext(ctx, `SELECT `+itemCols+itemFrom+` WHERE f.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, model.ErrNotFound
	}
	return r, err
}

// requireVisible returns ErrNotFound when viewer may not see the item.
func (s *Service) requireVisible(ctx context.Context, viewer, id string) error {
	ok, err := s.Vis.CanSeeFeedItem(ctx, viewer, id)
	if err != nil {
		return err
	}
	if !ok {
		return model.ErrNotFound
	}
	return nil
}

// Get returns one item as seen by viewer.
func (s *Service) Get(ctx context.Context, viewer, id string) (model.FeedItem, error) {
	if err := s.requireVisible(ctx, viewer, id); err != nil {
		return model.FeedItem{}, err
	}
	r, err := s.loadRow(ctx, id)
	if err != nil {
		return model.FeedItem{}, err
	}
	items, err := s.render(ctx, viewer, []itemRow{r})
	if err != nil {
		return model.FeedItem{}, err
	}
	return items[0], nil
}

// snapshot loads an item once and renders it per viewer (WS fan-out).
func (s *Service) snapshot(ctx context.Context, id string) (func(viewer string) model.FeedItem, *Reactors, error) {
	r, err := s.loadRow(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	u, err := s.Users.Get(ctx, r.user)
	if err != nil {
		return nil, nil, err
	}
	reacts, err := s.reactions(ctx, []string{id})
	if err != nil {
		return nil, nil, err
	}
	counts, err := s.commentCounts(ctx, []string{id})
	if err != nil {
		return nil, nil, err
	}
	base := r.base(u)
	base.CommentCount = counts[id]
	rs := reacts[id]
	return func(viewer string) model.FeedItem {
		it := base
		it.Reactions = rs.For(viewer)
		return it
	}, rs, nil
}

// PublishItem announces a new or updated item to everyone allowed to see it.
func (s *Service) PublishItem(ctx context.Context, id string) error {
	aud, err := s.Vis.FeedItemAudience(ctx, id)
	if err != nil {
		return err
	}
	render, _, err := s.snapshot(ctx, id)
	if err != nil {
		return err
	}
	s.Pub.Publish(events.Event{Type: "feed_item", Allow: events.Set(aud), Data: func(v string) any { return render(v) }})
	return nil
}

func (s *Service) publishReactions(ctx context.Context, id string) error {
	aud, err := s.Vis.FeedItemAudience(ctx, id)
	if err != nil {
		return err
	}
	reacts, err := s.reactions(ctx, []string{id})
	if err != nil {
		return err
	}
	rs := reacts[id]
	s.Pub.Publish(events.Event{Type: "reaction", Allow: events.Set(aud), Data: func(v string) any {
		return map[string]any{"item_id": id, "reactions": rs.For(v)}
	}})
	return nil
}

// ValidEmoji approximates "one grapheme": 1..32 bytes of valid UTF-8, at
// most 10 code points, no whitespace/control, not plain ASCII text.
func ValidEmoji(e string) bool {
	if e == "" || len(e) > 32 || !utf8.ValidString(e) || utf8.RuneCountInString(e) > 10 {
		return false
	}
	ascii := true
	for _, r := range e {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
		if r > unicode.MaxASCII {
			ascii = false
		}
	}
	return !ascii
}

// React adds viewer's reaction (idempotent) and returns the item.
func (s *Service) React(ctx context.Context, viewer, id, emoji string, add bool) (model.FeedItem, error) {
	if !ValidEmoji(emoji) {
		return model.FeedItem{}, model.ErrInvalid
	}
	if err := s.requireVisible(ctx, viewer, id); err != nil {
		return model.FeedItem{}, err
	}
	var res sql.Result
	var err error
	if add {
		res, err = s.DB.W.ExecContext(ctx, `INSERT OR IGNORE INTO reactions (item_id, user_id, emoji, created_at) VALUES (?, ?, ?, ?)`,
			id, viewer, emoji, s.Now().UnixMilli())
	} else {
		res, err = s.DB.W.ExecContext(ctx, `DELETE FROM reactions WHERE item_id = ? AND user_id = ? AND emoji = ?`, id, viewer, emoji)
	}
	if err != nil {
		return model.FeedItem{}, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		if err := s.publishReactions(ctx, id); err != nil {
			return model.FeedItem{}, err
		}
	}
	return s.Get(ctx, viewer, id)
}

const commentCols = `c.id, c.item_id, c.user_id, c.body, c.created_at, c.edited_at`

type commentRow struct {
	id      int64
	item    string
	user    string
	body    string
	created int64
	edited  sql.NullInt64
}

func scanComment(sc users.Scanner) (commentRow, error) {
	var c commentRow
	err := sc.Scan(&c.id, &c.item, &c.user, &c.body, &c.created, &c.edited)
	return c, err
}

func (c commentRow) render(u model.User) model.Comment {
	out := model.Comment{ID: strconv.FormatInt(c.id, 10), User: u, Body: c.body, CreatedAt: model.TS(c.created)}
	if c.edited.Valid {
		out.EditedAt = model.TSPtr(&c.edited.Int64)
	}
	return out
}

// Comments lists the item's comments, oldest first.
func (s *Service) Comments(ctx context.Context, viewer, id string) ([]model.Comment, error) {
	if err := s.requireVisible(ctx, viewer, id); err != nil {
		return nil, err
	}
	rows, err := s.DB.R.QueryContext(ctx, `SELECT `+commentCols+` FROM comments c WHERE c.item_id = ? ORDER BY c.id`, id)
	if err != nil {
		return nil, err
	}
	var list []commentRow
	var names []string
	for rows.Next() {
		c, err := scanComment(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, c)
		names = append(names, c.user)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	us, err := s.Users.GetMany(ctx, names)
	if err != nil {
		return nil, err
	}
	out := make([]model.Comment, 0, len(list))
	for _, c := range list {
		out = append(out, c.render(us[c.user]))
	}
	return out, nil
}

// ValidBody checks a comment body: 1..2000 characters after trimming.
func ValidBody(b string) (string, bool) {
	b = strings.TrimSpace(b)
	n := utf8.RuneCountInString(b)
	return b, n >= 1 && n <= 2000 && utf8.ValidString(b)
}

func (s *Service) loadComment(ctx context.Context, cid string) (commentRow, error) {
	n, err := strconv.ParseInt(cid, 10, 64)
	if err != nil {
		return commentRow{}, model.ErrNotFound
	}
	c, err := scanComment(s.DB.R.QueryRowContext(ctx, `SELECT `+commentCols+` FROM comments c WHERE c.id = ?`, n))
	if errors.Is(err, sql.ErrNoRows) {
		return c, model.ErrNotFound
	}
	return c, err
}

func (s *Service) publishComment(ctx context.Context, c commentRow) (model.Comment, error) {
	u, err := s.Users.Get(ctx, c.user)
	if err != nil {
		return model.Comment{}, err
	}
	out := c.render(u)
	aud, err := s.Vis.FeedItemAudience(ctx, c.item)
	if err != nil {
		return out, err
	}
	s.Pub.Publish(events.Event{Type: "comment", Allow: events.Set(aud),
		Data: events.Same(map[string]any{"item_id": c.item, "comment": out})})
	return out, nil
}

// AddComment posts a comment on an item viewer can see.
func (s *Service) AddComment(ctx context.Context, viewer, id, body string) (model.Comment, error) {
	body, ok := ValidBody(body)
	if !ok {
		return model.Comment{}, model.ErrInvalid
	}
	if err := s.requireVisible(ctx, viewer, id); err != nil {
		return model.Comment{}, err
	}
	res, err := s.DB.W.ExecContext(ctx, `INSERT INTO comments (item_id, user_id, body, created_at) VALUES (?, ?, ?, ?)`,
		id, viewer, body, s.Now().UnixMilli())
	if err != nil {
		return model.Comment{}, err
	}
	cid, _ := res.LastInsertId()
	c, err := s.loadComment(ctx, strconv.FormatInt(cid, 10))
	if err != nil {
		return model.Comment{}, err
	}
	return s.publishComment(ctx, c)
}

// EditComment changes the body (author only).
func (s *Service) EditComment(ctx context.Context, viewer, cid, body string) (model.Comment, error) {
	body, ok := ValidBody(body)
	if !ok {
		return model.Comment{}, model.ErrInvalid
	}
	c, err := s.loadComment(ctx, cid)
	if err != nil {
		return model.Comment{}, err
	}
	if c.user != viewer {
		// Do not reveal comments on items the viewer cannot see.
		if ok, _ := s.Vis.CanSeeFeedItem(ctx, viewer, c.item); !ok {
			return model.Comment{}, model.ErrNotFound
		}
		return model.Comment{}, model.ErrForbidden
	}
	if _, err := s.DB.W.ExecContext(ctx, `UPDATE comments SET body = ?, edited_at = ? WHERE id = ?`,
		body, s.Now().UnixMilli(), c.id); err != nil {
		return model.Comment{}, err
	}
	c, err = s.loadComment(ctx, cid)
	if err != nil {
		return model.Comment{}, err
	}
	return s.publishComment(ctx, c)
}

// DeleteComment removes a comment: its author, or an owner/admin of a circle
// that makes the item visible.
func (s *Service) DeleteComment(ctx context.Context, viewer, cid string) error {
	c, err := s.loadComment(ctx, cid)
	if err != nil {
		return err
	}
	if c.user != viewer {
		ok, err := s.Vis.CanModerateFeedItem(ctx, viewer, c.item)
		if err != nil {
			return err
		}
		if !ok {
			if see, _ := s.Vis.CanSeeFeedItem(ctx, viewer, c.item); !see {
				return model.ErrNotFound
			}
			return model.ErrForbidden
		}
	}
	// Compute the audience before deleting so the event reaches the same people.
	aud, err := s.Vis.FeedItemAudience(ctx, c.item)
	if err != nil {
		return err
	}
	if _, err := s.DB.W.ExecContext(ctx, `DELETE FROM comments WHERE id = ?`, c.id); err != nil {
		return err
	}
	s.Pub.Publish(events.Event{Type: "comment_deleted", Allow: events.Set(aud),
		Data: events.Same(map[string]any{"item_id": c.item, "comment_id": strconv.FormatInt(c.id, 10)})})
	return nil
}

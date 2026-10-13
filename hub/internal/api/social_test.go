package api

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

type itemResp struct {
	ID        string `json:"id"`
	Reactions []struct {
		Emoji string `json:"emoji"`
		Count int    `json:"count"`
		Mine  bool   `json:"mine"`
	} `json:"reactions"`
	CommentCount int `json:"comment_count"`
}

type commentResp struct {
	ID       string  `json:"id"`
	Body     string  `json:"body"`
	EditedAt *string `json:"edited_at"`
	User     struct {
		Username string `json:"username"`
	} `json:"user"`
}

func TestReactionsAndComments(t *testing.T) {
	e := newEnv(t)
	login(e, "alice", "bob", "carol", "dave")
	e.leaveEveryone("carol")
	e.scrobble("alice", track("t1", "al1", "ar1"), t0.Add(-10*time.Minute))
	item := listened(e.feed("alice", ""))[0].ID
	fire := "/api/v1/feed/" + item + "/reactions/" + url.PathEscape("🔥")

	var it itemResp
	e.must(200, "PUT", fire, "bob", nil, &it)
	e.must(200, "PUT", fire, "bob", nil, &it) // idempotent
	if len(it.Reactions) != 1 || it.Reactions[0].Count != 1 || !it.Reactions[0].Mine || it.Reactions[0].Emoji != "🔥" {
		t.Fatalf("reactions: %+v", it.Reactions)
	}
	e.must(200, "PUT", fire, "alice", nil, &it)
	if it.Reactions[0].Count != 2 {
		t.Fatalf("two reactors: %+v", it.Reactions)
	}
	e.must(200, "DELETE", fire, "alice", nil, &it)
	if it.Reactions[0].Count != 1 || it.Reactions[0].Mine {
		t.Fatalf("after alice removes: %+v", it.Reactions)
	}
	if code := e.req("PUT", fire, "carol", nil, nil); code != 404 {
		t.Fatalf("carol cannot see the item, react: %d", code)
	}
	if code := e.req("PUT", "/api/v1/feed/"+item+"/reactions/abc", "bob", nil, nil); code != 400 {
		t.Fatalf("not an emoji: %d", code)
	}

	comments := "/api/v1/feed/" + item + "/comments"
	var c commentResp
	e.must(201, "POST", comments, "bob", map[string]string{"body": "  great album  "}, &c)
	if c.Body != "great album" || c.User.Username != "bob" || c.EditedAt != nil {
		t.Fatalf("comment: %+v", c)
	}
	if code := e.req("POST", comments, "bob", map[string]string{"body": "   "}, nil); code != 400 {
		t.Fatalf("empty body: %d", code)
	}
	if code := e.req("POST", comments, "bob", map[string]string{"body": strings.Repeat("é", 2001)}, nil); code != 400 {
		t.Fatalf("2001 chars: %d", code)
	}
	if code := e.req("POST", comments, "carol", map[string]string{"body": "hi"}, nil); code != 404 {
		t.Fatalf("carol comment: %d", code)
	}
	if code := e.req("GET", comments, "carol", nil, nil); code != 404 {
		t.Fatalf("carol list comments: %d", code)
	}
	var list struct {
		Items []commentResp `json:"items"`
	}
	e.must(200, "GET", comments, "alice", nil, &list)
	if len(list.Items) != 1 {
		t.Fatalf("comments: %+v", list)
	}
	e.must(200, "GET", "/api/v1/feed?limit=200", "alice", nil, nil)
	for _, f := range e.feed("alice", "") {
		if f.ID == item {
			var got itemResp
			e.must(200, "PUT", fire, "alice", nil, &got)
			if got.CommentCount != 1 {
				t.Fatalf("comment_count: %d", got.CommentCount)
			}
		}
	}

	if code := e.req("PATCH", "/api/v1/comments/"+c.ID, "alice", map[string]string{"body": "edited"}, nil); code != 403 {
		t.Fatalf("only the author edits: %d", code)
	}
	if code := e.req("PATCH", "/api/v1/comments/"+c.ID, "carol", map[string]string{"body": "edited"}, nil); code != 404 {
		t.Fatalf("strangers get 404: %d", code)
	}
	e.must(200, "PATCH", "/api/v1/comments/"+c.ID, "bob", map[string]string{"body": "edited"}, &c)
	if c.Body != "edited" || c.EditedAt == nil {
		t.Fatalf("edit: %+v", c)
	}

	// Deletion: not the item owner in `everyone` (no admins there), not strangers.
	if code := e.req("DELETE", "/api/v1/comments/"+c.ID, "alice", nil, nil); code != 403 {
		t.Fatalf("non-author non-admin delete: %d", code)
	}
	if code := e.req("DELETE", "/api/v1/comments/"+c.ID, "carol", nil, nil); code != 404 {
		t.Fatalf("stranger delete: %d", code)
	}
	// dave owns a circle in which alice's item is visible → may moderate.
	e.createCircle("dave", "Mods", "alice")
	e.must(204, "DELETE", "/api/v1/comments/"+c.ID, "dave", nil, nil)

	var c2 commentResp
	e.must(201, "POST", comments, "bob", map[string]string{"body": "again"}, &c2)
	e.must(204, "DELETE", "/api/v1/comments/"+c2.ID, "bob", nil, nil)
}

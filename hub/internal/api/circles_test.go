package api

import (
	"testing"
	"time"
)

type inviteResp struct {
	Code    string `json:"code"`
	MaxUses int    `json:"max_uses"`
	Uses    int    `json:"uses"`
	URLPath string `json:"url_path"`
}

func TestInvitesExpiryAndMaxUses(t *testing.T) {
	e := newEnv(t)
	login(e, "alice", "bob", "carol", "dave")
	fam := e.createCircle("alice", "Family")

	var one inviteResp
	e.must(201, "POST", "/api/v1/circles/"+fam+"/invites", "alice", map[string]any{"max_uses": 1}, &one)
	if one.URLPath != "/cercle/join/"+one.Code {
		t.Fatalf("url_path %q", one.URLPath)
	}
	e.must(200, "POST", "/api/v1/invites/"+one.Code+"/accept", "bob", nil, nil)
	e.must(200, "POST", "/api/v1/invites/"+one.Code+"/accept", "bob", nil, nil) // idempotent, no use consumed
	if code := e.req("POST", "/api/v1/invites/"+one.Code+"/accept", "carol", nil, nil); code != 410 {
		t.Fatalf("max uses reached: %d, want 410", code)
	}
	var info struct {
		Valid  bool `json:"valid"`
		Circle struct {
			MemberCount int `json:"member_count"`
		} `json:"circle"`
		CreatedBy struct {
			Username string `json:"username"`
		} `json:"created_by"`
	}
	e.must(200, "GET", "/api/v1/invites/"+one.Code, "carol", nil, &info)
	if info.Valid || info.Circle.MemberCount != 2 || info.CreatedBy.Username != "alice" {
		t.Fatalf("invite info: %+v", info)
	}

	var short inviteResp
	e.must(201, "POST", "/api/v1/circles/"+fam+"/invites", "alice", map[string]any{"expires_in_hours": 1}, &short)
	e.must(200, "GET", "/api/v1/invites/"+short.Code, "carol", nil, &info)
	if !info.Valid {
		t.Fatal("fresh invite is valid")
	}
	e.clock.Advance(61 * time.Minute)
	if code := e.req("POST", "/api/v1/circles/"+fam+"/join", "carol", map[string]string{"code": short.Code}, nil); code != 410 {
		t.Fatalf("expired invite: %d, want 410", code)
	}

	var unlimited inviteResp
	e.must(201, "POST", "/api/v1/circles/"+fam+"/invites", "alice", nil, &unlimited)
	if code := e.req("POST", "/api/v1/circles/"+fam+"/join", "carol", nil, nil); code != 400 {
		t.Fatalf("join without code: %d", code)
	}
	other := e.createCircle("dave", "Other")
	if code := e.req("POST", "/api/v1/circles/"+other+"/join", "carol", map[string]string{"code": unlimited.Code}, nil); code != 404 {
		t.Fatalf("code of another circle: %d", code)
	}
	e.must(200, "POST", "/api/v1/circles/"+fam+"/join", "carol", map[string]string{"code": unlimited.Code}, nil)
	e.must(200, "POST", "/api/v1/circles/"+fam+"/join", "dave", map[string]string{"code": unlimited.Code}, nil)

	if code := e.req("GET", "/api/v1/circles/"+fam+"/invites", "carol", nil, nil); code != 403 {
		t.Fatalf("members cannot list invites: %d", code)
	}
	var list struct {
		Items []inviteResp `json:"items"`
	}
	e.must(200, "GET", "/api/v1/circles/"+fam+"/invites", "alice", nil, &list)
	if len(list.Items) != 3 {
		t.Fatalf("invites: %+v", list.Items)
	}
	e.must(204, "DELETE", "/api/v1/circles/"+fam+"/invites/"+unlimited.Code, "alice", nil, nil)
	if code := e.req("GET", "/api/v1/invites/"+unlimited.Code, "carol", nil, nil); code != 404 {
		t.Fatalf("deleted invite: %d", code)
	}
}

func TestCircleRolesAndLeaving(t *testing.T) {
	e := newEnv(t)
	login(e, "alice", "bob", "carol")
	fam := e.createCircle("alice", "Family", "bob", "carol")

	var detail struct {
		MyRole  *string `json:"my_role"`
		Members []struct {
			User struct {
				Username string `json:"username"`
			} `json:"user"`
			Role    string          `json:"role"`
			Sharing *map[string]any `json:"sharing"`
		} `json:"members"`
	}
	e.must(200, "GET", "/api/v1/circles/"+fam, "carol", nil, &detail)
	for _, m := range detail.Members {
		if (m.User.Username == "carol") != (m.Sharing != nil) {
			t.Fatalf("members see only their own sharing flags: %+v", m)
		}
	}
	if code := e.req("GET", "/api/v1/circles/"+fam, "dave", nil, nil); code != 404 {
		t.Fatalf("non-member circle detail: %d", code)
	}

	e.must(200, "PUT", "/api/v1/circles/"+fam+"/members/bob", "alice", map[string]string{"role": "admin"}, nil)
	if code := e.req("PUT", "/api/v1/circles/"+fam+"/members/carol", "bob", map[string]string{"role": "admin"}, nil); code != 403 {
		t.Fatalf("only the owner changes roles: %d", code)
	}
	e.must(201, "POST", "/api/v1/circles/"+fam+"/invites", "bob", nil, nil)
	e.must(200, "PATCH", "/api/v1/circles/"+fam, "bob", map[string]string{"name": "Fam"}, nil)
	if code := e.req("PATCH", "/api/v1/circles/"+fam, "carol", map[string]string{"name": "x"}, nil); code != 403 {
		t.Fatalf("member rename: %d", code)
	}
	e.must(200, "GET", "/api/v1/circles/"+fam, "bob", nil, &detail)
	for _, m := range detail.Members {
		if m.Sharing == nil {
			t.Fatal("admins see everyone's sharing flags")
		}
	}

	if code := e.req("DELETE", "/api/v1/circles/"+fam+"/members/alice", "bob", nil, nil); code != 403 {
		t.Fatalf("cannot remove the owner: %d", code)
	}
	if code := e.req("POST", "/api/v1/circles/"+fam+"/leave", "alice", nil, nil); code != 409 {
		t.Fatalf("owner must transfer first: %d", code)
	}
	if code := e.req("DELETE", "/api/v1/circles/"+fam, "bob", nil, nil); code != 403 {
		t.Fatalf("only the owner deletes: %d", code)
	}
	e.must(200, "PUT", "/api/v1/circles/"+fam+"/members/bob", "alice", map[string]string{"role": "owner"}, nil)
	e.must(200, "GET", "/api/v1/circles/"+fam, "alice", nil, &detail)
	if detail.MyRole == nil || *detail.MyRole != "admin" {
		t.Fatalf("previous owner becomes admin: %v", detail.MyRole)
	}
	e.must(204, "DELETE", "/api/v1/circles/"+fam+"/members/carol", "alice", nil, nil)
	e.must(204, "POST", "/api/v1/circles/"+fam+"/leave", "alice", nil, nil)
	e.must(204, "POST", "/api/v1/circles/"+fam+"/leave", "bob", nil, nil) // last member: circle deleted
	if code := e.req("GET", "/api/v1/circles/"+fam, "bob", nil, nil); code != 404 {
		t.Fatalf("circle deleted with its last member: %d", code)
	}
	var n int
	if err := e.srv.DB.R.QueryRow(`SELECT COUNT(*) FROM circles WHERE id = ?`, fam).Scan(&n); err != nil || n != 0 {
		t.Fatal("circle row should be gone")
	}
}

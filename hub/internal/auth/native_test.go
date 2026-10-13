package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNativeListUsers(t *testing.T) {
	var total string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/music/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "jwt", "isAdmin": true, "subsonicToken": "x"})
		case "/music/api/user":
			if r.Header.Get("X-ND-Authorization") != "Bearer jwt" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("X-Total-Count", total)
			_, _ = w.Write([]byte(`[{"id":"1","userName":"alice","name":"Alice","isAdmin":true},{"id":"2","userName":"bob","name":"","isAdmin":false}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	n := NewNativeNavidrome(srv.URL+"/music/", "admin", "s3cret")
	total = "2"
	users, err := n.ListUsers(context.Background())
	if err != nil || len(users) != 2 || users[0] != (NavUser{ID: "1", Username: "alice", Name: "Alice", IsAdmin: true}) {
		t.Fatalf("list: %+v %v", users, err)
	}
	total = "3" // a partial page must never drive disabling
	if _, err := n.ListUsers(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("partial page: %v", err)
	}
}

func TestNativeErrorsNeverLeakPassword(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	n := NewNativeNavidrome(srv.URL, "admin", "s3cret-pass")
	_, err := n.ListUsers(context.Background())
	if !errors.Is(err, ErrBadCredentials) || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("bad creds: %v", err)
	}
	srv.Close()
	_, err = n.ListUsers(context.Background())
	if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("down: %v", err)
	}
}

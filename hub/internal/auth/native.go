package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrNotAdmin means the sync account is not a Navidrome admin.
var ErrNotAdmin = errors.New("navidrome sync account is not an admin")

// NavUser is one Navidrome user as listed by the native API.
type NavUser struct {
	ID       string
	Username string
	Name     string
	IsAdmin  bool
}

// UserLister lists every Navidrome user.
type UserLister interface {
	ListUsers(ctx context.Context) ([]NavUser, error)
}

// NativeNavidrome lists users through Navidrome's native (UI) API:
// POST /auth/login → JWT, then GET /api/user with X-ND-Authorization.
// (Subsonic getUsers only returns the calling user on Navidrome 0.64.)
// The admin password travels only in the login POST body and is never logged;
// the JWT is cached until Navidrome rejects it.
type NativeNavidrome struct {
	BaseURL  string
	User     string
	Password string
	Client   *http.Client

	mu    sync.Mutex
	token string
}

func NewNativeNavidrome(base, user, password string) *NativeNavidrome {
	return &NativeNavidrome{BaseURL: strings.TrimRight(base, "/"), User: user, Password: password,
		Client: &http.Client{Timeout: 15 * time.Second}}
}

func transportErr(op string, err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return fmt.Errorf("%w: %s: %v", ErrUnavailable, op, err)
}

func (n *NativeNavidrome) login(ctx context.Context) (string, error) {
	body, _ := json.Marshal(map[string]string{"username": n.User, "password": n.Password})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.BaseURL+"/auth/login", bytes.NewReader(body))
	if err != nil {
		return "", transportErr("POST /auth/login", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.Client.Do(req)
	if err != nil {
		return "", transportErr("POST /auth/login", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return "", fmt.Errorf("%w: POST /auth/login: HTTP %d", ErrBadCredentials, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return "", fmt.Errorf("%w: POST /auth/login: HTTP %d", ErrUnavailable, resp.StatusCode)
	}
	// The payload also holds subsonicToken/subsonicSalt: decode only what we need.
	var out struct {
		Token   string `json:"token"`
		IsAdmin bool   `json:"isAdmin"`
	}
	if err := json.Unmarshal(data, &out); err != nil || out.Token == "" {
		return "", fmt.Errorf("%w: POST /auth/login: unexpected response", ErrUnavailable)
	}
	if !out.IsAdmin {
		return "", ErrNotAdmin
	}
	return out.Token, nil
}

type nativeUser struct {
	ID       string `json:"id"`
	UserName string `json:"userName"`
	Name     string `json:"name"`
	IsAdmin  bool   `json:"isAdmin"`
}

// listOnce returns the users, or retry=true when the token was rejected.
func (n *NativeNavidrome) listOnce(ctx context.Context, tok string) (users []NavUser, retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.BaseURL+"/api/user?_sort=userName&_order=ASC", nil)
	if err != nil {
		return nil, false, transportErr("GET /api/user", err)
	}
	req.Header.Set("X-ND-Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	resp, err := n.Client.Do(req)
	if err != nil {
		return nil, false, transportErr("GET /api/user", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, true, nil
	case resp.StatusCode == http.StatusForbidden:
		return nil, false, ErrNotAdmin
	case resp.StatusCode != http.StatusOK:
		return nil, false, fmt.Errorf("%w: GET /api/user: HTTP %d", ErrUnavailable, resp.StatusCode)
	}
	var list []nativeUser
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&list); err != nil {
		return nil, false, fmt.Errorf("%w: GET /api/user: not a user list", ErrUnavailable)
	}
	// Refuse a partial page: the listing drives disabling.
	if tc := resp.Header.Get("X-Total-Count"); tc != "" {
		if total, err := strconv.Atoi(tc); err == nil && total != len(list) {
			return nil, false, fmt.Errorf("%w: GET /api/user returned %d of %d users", ErrUnavailable, len(list), total)
		}
	}
	// Navidrome slides the session by returning a fresh token.
	if fresh := resp.Header.Get("X-ND-Authorization"); fresh != "" {
		n.mu.Lock()
		n.token = strings.TrimSpace(strings.TrimPrefix(fresh, "Bearer "))
		n.mu.Unlock()
	}
	out := make([]NavUser, 0, len(list))
	for _, u := range list {
		if u.UserName != "" {
			out = append(out, NavUser{ID: u.ID, Username: u.UserName, Name: u.Name, IsAdmin: u.IsAdmin})
		}
	}
	return out, false, nil
}

// ListUsers implements UserLister: cached JWT, one re-login on 401.
func (n *NativeNavidrome) ListUsers(ctx context.Context) ([]NavUser, error) {
	n.mu.Lock()
	tok := n.token
	n.mu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		if tok == "" {
			var err error
			if tok, err = n.login(ctx); err != nil {
				return nil, err
			}
			n.mu.Lock()
			n.token = tok
			n.mu.Unlock()
		}
		users, retry, err := n.listOnce(ctx, tok)
		if err != nil {
			if errors.Is(err, ErrNotAdmin) {
				n.mu.Lock()
				n.token = ""
				n.mu.Unlock()
			}
			return nil, err
		}
		if !retry {
			return users, nil
		}
		n.mu.Lock()
		n.token = ""
		n.mu.Unlock()
		tok = ""
	}
	return nil, fmt.Errorf("%w: GET /api/user: token rejected after re-login", ErrBadCredentials)
}

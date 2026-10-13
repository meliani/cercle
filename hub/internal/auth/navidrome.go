// Package auth validates users against Navidrome and manages hub sessions
// (docs/CONTRACT.md §3).
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var (
	// ErrBadCredentials: Navidrome rejected the credentials.
	ErrBadCredentials = errors.New("bad credentials")
	// ErrUnavailable: Navidrome could not be reached or answered garbage.
	ErrUnavailable = errors.New("navidrome unavailable")
)

// Creds are Subsonic credentials in one of three forms: token+salt,
// password, or API key.
type Creds struct {
	Username string
	Token    string
	Salt     string
	Password string
	APIKey   string
}

// Valid reports whether exactly one credential form is present.
func (c Creds) Valid() bool {
	n := 0
	if c.Token != "" && c.Salt != "" {
		n++
	}
	if c.Password != "" {
		n++
	}
	if c.APIKey != "" {
		n++
	}
	return n == 1 && (c.Username != "" || c.APIKey != "")
}

func (c Creds) params() url.Values {
	v := url.Values{}
	switch {
	case c.APIKey != "":
		v.Set("apiKey", c.APIKey)
	case c.Password != "":
		v.Set("u", c.Username)
		v.Set("p", c.Password)
	default:
		v.Set("u", c.Username)
		v.Set("t", c.Token)
		v.Set("s", c.Salt)
	}
	v.Set("v", "1.16.1")
	v.Set("c", "cercle-hub")
	v.Set("f", "json")
	return v
}

// Identity is what a successful validation tells us.
type Identity struct {
	Username string
	IsAdmin  bool
}

// Navidrome validates credentials. Tests substitute a fake server via
// HTTPNavidrome pointed at httptest, or another implementation.
type Navidrome interface {
	Validate(ctx context.Context, c Creds) (Identity, error)
}

// HTTPNavidrome talks to a real Navidrome over the Subsonic API.
type HTTPNavidrome struct {
	BaseURL string
	Client  *http.Client
}

func NewHTTPNavidrome(base string) *HTTPNavidrome {
	return &HTTPNavidrome{BaseURL: strings.TrimRight(base, "/"), Client: &http.Client{Timeout: 10 * time.Second}}
}

type subsonicError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type subsonicEnvelope struct {
	Resp struct {
		Status string         `json:"status"`
		Error  *subsonicError `json:"error"`
		User   *struct {
			Username  string `json:"username"`
			AdminRole bool   `json:"adminRole"`
		} `json:"user"`
		TokenInfo *struct {
			Username string `json:"username"`
		} `json:"tokenInfo"`
	} `json:"subsonic-response"`
}

func (n *HTTPNavidrome) call(ctx context.Context, endpoint string, v url.Values) (*subsonicEnvelope, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.BaseURL+"/rest/"+endpoint+".view?"+v.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := n.Client.Do(req)
	if err != nil {
		// Never log the URL: its query string carries the user's credentials.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("%w: GET /rest/%s.view: %v", ErrUnavailable, endpoint, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, ErrBadCredentials
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: HTTP %d", ErrUnavailable, resp.StatusCode)
	}
	var env subsonicEnvelope
	if err := json.Unmarshal(body, &env); err != nil || env.Resp.Status == "" {
		return nil, fmt.Errorf("%w: not a subsonic response", ErrUnavailable)
	}
	if env.Resp.Status != "ok" {
		code := 0
		if env.Resp.Error != nil {
			code = env.Resp.Error.Code
		}
		switch code {
		case 10, 40, 41, 42, 43, 44, 50:
			return &env, ErrBadCredentials
		}
		return &env, fmt.Errorf("%w: subsonic error %d", ErrUnavailable, code)
	}
	return &env, nil
}

// Validate implements Navidrome. For token/password it calls ping; for an
// API key it resolves the owner via the OpenSubsonic tokenInfo endpoint and,
// if a username was claimed, requires it to match. adminRole comes from
// getUser (best effort: a failure there means "not admin").
func (n *HTTPNavidrome) Validate(ctx context.Context, c Creds) (Identity, error) {
	if !c.Valid() {
		return Identity{}, ErrBadCredentials
	}
	p := c.params()
	id := Identity{Username: c.Username}
	if c.APIKey != "" {
		env, err := n.call(ctx, "tokenInfo", p)
		if err != nil {
			return Identity{}, err
		}
		if env.Resp.TokenInfo == nil || env.Resp.TokenInfo.Username == "" {
			return Identity{}, ErrBadCredentials
		}
		if c.Username != "" && !strings.EqualFold(c.Username, env.Resp.TokenInfo.Username) {
			return Identity{}, ErrBadCredentials
		}
		id.Username = env.Resp.TokenInfo.Username
	} else if _, err := n.call(ctx, "ping", p); err != nil {
		return Identity{}, err
	}
	gp := c.params()
	gp.Set("username", id.Username)
	if env, err := n.call(ctx, "getUser", gp); err == nil && env.Resp.User != nil {
		id.IsAdmin = env.Resp.User.AdminRole
		if env.Resp.User.Username != "" {
			id.Username = env.Resp.User.Username
		}
	} else if errors.Is(err, ErrUnavailable) {
		return Identity{}, err
	}
	return id, nil
}

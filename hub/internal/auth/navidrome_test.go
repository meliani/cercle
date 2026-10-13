package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestErrorsNeverLeakCredentials(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // connection refused from now on
	n := NewHTTPNavidrome(url)
	_, err := n.Validate(context.Background(), Creds{Username: "alice", Password: "s3cret-pass"})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
	if strings.Contains(err.Error(), "s3cret-pass") || strings.Contains(err.Error(), "p=") {
		t.Fatalf("error leaks credentials: %v", err)
	}
	_, err = n.Validate(context.Background(), Creds{APIKey: "k3y-value"})
	if err == nil || strings.Contains(err.Error(), "k3y-value") {
		t.Fatalf("error leaks api key: %v", err)
	}
}

func TestCredsValid(t *testing.T) {
	for _, c := range []struct {
		c  Creds
		ok bool
	}{
		{Creds{Username: "a", Password: "p"}, true},
		{Creds{Username: "a", Token: "t", Salt: "s"}, true},
		{Creds{APIKey: "k"}, true},
		{Creds{Username: "a", Token: "t"}, false},
		{Creds{Username: "a", Password: "p", APIKey: "k"}, false},
		{Creds{Password: "p"}, false},
	} {
		if c.c.Valid() != c.ok {
			t.Errorf("%+v: Valid() = %v", c.c, !c.ok)
		}
	}
}

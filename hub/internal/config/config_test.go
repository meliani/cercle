package config

import (
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestFromEnv(t *testing.T) {
	c, err := FromEnv(env(map[string]string{
		"NAVIDROME_URL": "http://navidrome.example.com:4533/", "INGEST_SECRET": strings.Repeat("x", 32),
		"CERCLE_BASE_PATH": "hub/", "CERCLE_CORS_ORIGINS": "https://a.example.com, app://feishin",
		"TRUSTED_HEADER": "X-User", "TRUSTED_PROXIES": "192.0.2.0/24, 127.0.0.1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8080" || c.DBPath != "/data/cercle.db" || c.BasePath != "/hub" ||
		c.NavidromeURL != "http://navidrome.example.com:4533" || len(c.CORSOrigins) != 2 ||
		len(c.TrustedProxies) != 2 || c.Location.String() != "UTC" {
		t.Fatalf("%+v", c)
	}
	_, err = FromEnv(env(map[string]string{"INGEST_SECRET": "short"}))
	if err == nil || !strings.Contains(err.Error(), "NAVIDROME_URL") || !strings.Contains(err.Error(), "INGEST_SECRET") {
		t.Fatalf("want both errors, got %v", err)
	}
	if NormalizeBasePath("/") != "" || NormalizeBasePath("") != "" || NormalizeBasePath("/a/b/") != "/a/b" {
		t.Fatal("NormalizeBasePath")
	}
}

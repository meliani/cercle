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

func TestSyncExclusionAndNamingEnv(t *testing.T) {
	base := map[string]string{"NAVIDROME_URL": "http://navidrome.example.com", "INGEST_SECRET": strings.Repeat("x", 32)}
	with := func(kv ...string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	c, err := FromEnv(env(base))
	if err != nil || c.UserSyncEnabled() || c.UserSyncInterval.String() != "15m0s" || c.EveryoneName != "" {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	c, err = FromEnv(env(with("NAVIDROME_ADMIN_USER", "alice", "NAVIDROME_ADMIN_PASSWORD", "pw",
		"CERCLE_USER_SYNC_INTERVAL", "5m", "CERCLE_EXCLUDED_USERS", "Admin, svc-bot",
		"CERCLE_EVERYONE_NAME", "Famille", "CERCLE_EVERYONE_DESCRIPTION", "Toute la famille")))
	if err != nil || !c.UserSyncEnabled() || c.UserSyncInterval.Minutes() != 5 ||
		!c.IsExcluded("admin") || !c.IsExcluded("ADMIN") || !c.IsExcluded("svc-bot") || c.IsExcluded("alice") ||
		c.EveryoneName != "Famille" || c.EveryoneDescription != "Toute la famille" {
		t.Fatalf("set: %+v %v", c, err)
	}
	for _, bad := range []map[string]string{
		with("NAVIDROME_ADMIN_USER", "alice"),
		with("NAVIDROME_ADMIN_PASSWORD", "pw"),
		with("CERCLE_USER_SYNC_INTERVAL", "10s"),
		with("CERCLE_USER_SYNC_INTERVAL", "often"),
	} {
		if _, err := FromEnv(env(bad)); err == nil {
			t.Errorf("%v should fail", bad)
		}
	}
}

func TestAllowMassDisableEnv(t *testing.T) {
	m := map[string]string{"NAVIDROME_URL": "http://navidrome.example.com", "INGEST_SECRET": strings.Repeat("x", 32)}
	c, _ := FromEnv(env(m))
	if c.UserSyncAllowMassDisable {
		t.Fatal("off by default")
	}
	m["CERCLE_USER_SYNC_ALLOW_MASS_DISABLE"] = "true"
	if c, err := FromEnv(env(m)); err != nil || !c.UserSyncAllowMassDisable {
		t.Fatalf("true: %v", err)
	}
	m["CERCLE_USER_SYNC_ALLOW_MASS_DISABLE"] = "maybe"
	if _, err := FromEnv(env(m)); err == nil {
		t.Fatal("invalid bool must fail")
	}
}

// Package config reads the hub configuration from the environment
// (docs/CONTRACT.md §6).
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// Config is the validated hub configuration.
type Config struct {
	Listen         string
	DBPath         string
	NavidromeURL   string
	IngestSecret   string
	BasePath       string
	CORSOrigins    []string
	Location       *time.Location
	TrustedHeader  string
	TrustedProxies []netip.Prefix
	LogLevel       slog.Level

	// Navidrome user sync (enabled when both admin credentials are set).
	NavidromeAdminUser     string
	NavidromeAdminPassword string
	UserSyncInterval       time.Duration
	// UserSyncAllowMassDisable lifts the "at most max(1, 20%) disables per run" guard.
	UserSyncAllowMassDisable bool
	// ExcludedUsers holds lowercased usernames of service accounts.
	ExcludedUsers map[string]bool
	// EveryoneName / EveryoneDescription are applied at startup when non-empty.
	EveryoneName        string
	EveryoneDescription string
}

// UserSyncEnabled reports whether Navidrome user sync is configured.
func (c *Config) UserSyncEnabled() bool {
	return c.NavidromeAdminUser != "" && c.NavidromeAdminPassword != ""
}

// IsExcluded reports whether username is an excluded service account
// (case-insensitive).
func (c *Config) IsExcluded(username string) bool {
	return c.ExcludedUsers[strings.ToLower(strings.TrimSpace(username))]
}

// FromEnv builds a Config from getenv (usually os.Getenv).
func FromEnv(getenv func(string) string) (*Config, error) {
	c := &Config{
		Listen:        def(getenv("CERCLE_LISTEN"), ":8080"),
		DBPath:        def(getenv("CERCLE_DB"), "/data/cercle.db"),
		NavidromeURL:  strings.TrimRight(strings.TrimSpace(getenv("NAVIDROME_URL")), "/"),
		IngestSecret:  getenv("INGEST_SECRET"),
		BasePath:      NormalizeBasePath(getenv("CERCLE_BASE_PATH")),
		CORSOrigins:   splitList(getenv("CERCLE_CORS_ORIGINS")),
		TrustedHeader: strings.TrimSpace(getenv("TRUSTED_HEADER")),

		NavidromeAdminUser:     strings.TrimSpace(getenv("NAVIDROME_ADMIN_USER")),
		NavidromeAdminPassword: getenv("NAVIDROME_ADMIN_PASSWORD"),
		ExcludedUsers:          map[string]bool{},
		EveryoneName:           strings.TrimSpace(getenv("CERCLE_EVERYONE_NAME")),
		EveryoneDescription:    strings.TrimSpace(getenv("CERCLE_EVERYONE_DESCRIPTION")),
	}
	var errs []error
	if c.NavidromeURL == "" {
		errs = append(errs, errors.New("NAVIDROME_URL is required"))
	} else if !strings.HasPrefix(c.NavidromeURL, "http://") && !strings.HasPrefix(c.NavidromeURL, "https://") {
		errs = append(errs, errors.New("NAVIDROME_URL must start with http:// or https://"))
	}
	if len(c.IngestSecret) < 32 {
		errs = append(errs, errors.New("INGEST_SECRET is required and must be at least 32 characters"))
	}
	tz := def(getenv("TZ"), "UTC")
	loc, err := time.LoadLocation(tz)
	if err != nil {
		errs = append(errs, fmt.Errorf("TZ %q: %w", tz, err))
	}
	c.Location = loc
	for _, p := range splitList(getenv("TRUSTED_PROXIES")) {
		pfx, err := ParsePrefix(p)
		if err != nil {
			errs = append(errs, fmt.Errorf("TRUSTED_PROXIES %q: %w", p, err))
			continue
		}
		c.TrustedProxies = append(c.TrustedProxies, pfx)
	}
	if c.TrustedHeader != "" && len(c.TrustedProxies) == 0 {
		errs = append(errs, errors.New("TRUSTED_HEADER requires TRUSTED_PROXIES"))
	}
	if (c.NavidromeAdminUser == "") != (c.NavidromeAdminPassword == "") {
		errs = append(errs, errors.New("NAVIDROME_ADMIN_USER and NAVIDROME_ADMIN_PASSWORD must be set together"))
	}
	iv, err := time.ParseDuration(def(getenv("CERCLE_USER_SYNC_INTERVAL"), "15m"))
	if err != nil || iv < time.Minute {
		errs = append(errs, fmt.Errorf("CERCLE_USER_SYNC_INTERVAL %q: want a duration of at least 1m", getenv("CERCLE_USER_SYNC_INTERVAL")))
	}
	c.UserSyncInterval = iv
	if v := strings.TrimSpace(getenv("CERCLE_USER_SYNC_ALLOW_MASS_DISABLE")); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("CERCLE_USER_SYNC_ALLOW_MASS_DISABLE %q: want true or false", v))
		}
		c.UserSyncAllowMassDisable = b
	}
	for _, u := range splitList(getenv("CERCLE_EXCLUDED_USERS")) {
		c.ExcludedUsers[strings.ToLower(u)] = true
	}
	if n := len([]rune(c.EveryoneName)); n > 100 {
		errs = append(errs, errors.New("CERCLE_EVERYONE_NAME must be at most 100 characters"))
	}
	switch strings.ToLower(def(getenv("CERCLE_LOG_LEVEL"), "info")) {
	case "debug":
		c.LogLevel = slog.LevelDebug
	case "info":
		c.LogLevel = slog.LevelInfo
	case "warn", "warning":
		c.LogLevel = slog.LevelWarn
	case "error":
		c.LogLevel = slog.LevelError
	default:
		errs = append(errs, fmt.Errorf("CERCLE_LOG_LEVEL %q: want debug|info|warn|error", getenv("CERCLE_LOG_LEVEL")))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return c, nil
}

// NormalizeBasePath turns "hub/", "/hub/" or "/hub" into "/hub" and "/" into "".
func NormalizeBasePath(p string) string {
	p = strings.Trim(strings.TrimSpace(p), "/")
	if p == "" {
		return ""
	}
	return "/" + p
}

// ParsePrefix accepts a CIDR or a bare IP address.
func ParsePrefix(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		return p.Masked(), err
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(a, a.BitLen()), nil
}

func def(v, d string) string {
	if strings.TrimSpace(v) == "" {
		return d
	}
	return strings.TrimSpace(v)
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

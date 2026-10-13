// Package config reads the hub configuration from the environment
// (docs/CONTRACT.md §6).
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
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

package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dnscaleou/external-dns-webhook-dnscale/internal/provider"
	"github.com/google/uuid"
)

type Config struct {
	Token, BaseURL, Listen, HealthListen string
	APITimeout, RequestTimeout           time.Duration
	Provider                             provider.Options
}

func Load() (Config, error) { return load(os.Getenv, os.ReadFile) }

func load(get func(string) string, read func(string) ([]byte, error)) (Config, error) {
	c := Config{BaseURL: "https://api.dnscale.eu/v1", Listen: "127.0.0.1:8888", HealthListen: "0.0.0.0:8080", APITimeout: 15 * time.Second, RequestTimeout: 90 * time.Second}
	path := get("DNSCALE_API_TOKEN_FILE")
	if path == "" {
		return c, fmt.Errorf("DNSCALE_API_TOKEN_FILE is required")
	}
	data, err := read(path)
	if err != nil {
		return c, fmt.Errorf("cannot read API token file")
	}
	c.Token = strings.TrimSpace(string(data))
	if c.Token == "" || strings.ContainsAny(c.Token, "\r\n\t ") {
		return c, fmt.Errorf("API token file must contain one nonempty token")
	}
	if value := get("DNSCALE_API_URL"); value != "" {
		c.BaseURL = value
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return c, fmt.Errorf("invalid DNSCALE_API_URL")
	}
	// HTTP is restricted to explicitly enabled fixture testing.
	if u.Scheme != "https" && !(u.Scheme == "http" && get("DNSCALE_ALLOW_HTTP") == "true") {
		return c, fmt.Errorf("API URL must use HTTPS (DNSCALE_ALLOW_HTTP=true is for local fixtures only)")
	}
	for _, pair := range []struct {
		name string
		dest *string
	}{{"DNSCALE_LISTEN", &c.Listen}, {"DNSCALE_HEALTH_LISTEN", &c.HealthListen}} {
		if value := get(pair.name); value != "" {
			*pair.dest = value
		}
		if _, _, err := net.SplitHostPort(*pair.dest); err != nil {
			return c, fmt.Errorf("invalid %s", pair.name)
		}
	}
	host, _, _ := net.SplitHostPort(c.Listen)
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return c, fmt.Errorf("DNSCALE_LISTEN must bind a loopback address")
	}
	c.Provider = provider.Options{Domains: csv(get("DNSCALE_DOMAIN_FILTER")), ExcludeDomains: csv(get("DNSCALE_EXCLUDE_DOMAINS")), ZoneIDs: csv(get("DNSCALE_ZONE_ID_FILTER")), OwnerID: get("DNSCALE_TXT_OWNER_ID"), DefaultTTL: 300}
	c.Provider.DryRun = true
	if value := get("DNSCALE_DRY_RUN"); value != "" {
		c.Provider.DryRun, err = strconv.ParseBool(value)
		if err != nil {
			return c, fmt.Errorf("DNSCALE_DRY_RUN must be true or false")
		}
	}
	if len(c.Provider.Domains) == 0 || len(c.Provider.ZoneIDs) == 0 || c.Provider.OwnerID == "" {
		return c, fmt.Errorf("DNSCALE_DOMAIN_FILTER, DNSCALE_ZONE_ID_FILTER, and DNSCALE_TXT_OWNER_ID are required")
	}
	for _, id := range c.Provider.ZoneIDs {
		if value, err := uuid.Parse(id); err != nil || value == uuid.Nil {
			return c, fmt.Errorf("zone filters must contain nonzero UUIDs")
		}
	}
	if value := get("DNSCALE_DEFAULT_TTL"); value != "" {
		c.Provider.DefaultTTL, err = strconv.Atoi(value)
		if err != nil || c.Provider.DefaultTTL < 300 || c.Provider.DefaultTTL > 86400 {
			return c, fmt.Errorf("DNSCALE_DEFAULT_TTL must be between 300 and 86400")
		}
	}
	for _, pair := range []struct {
		name string
		dest *time.Duration
	}{{"DNSCALE_API_TIMEOUT", &c.APITimeout}, {"DNSCALE_REQUEST_TIMEOUT", &c.RequestTimeout}} {
		if value := get(pair.name); value != "" {
			*pair.dest, err = time.ParseDuration(value)
			if err != nil || *pair.dest < time.Second || *pair.dest > 10*time.Minute {
				return c, fmt.Errorf("%s must be between 1s and 10m", pair.name)
			}
		}
	}
	return c, nil
}

func csv(value string) []string {
	if value == "" {
		return nil
	}
	values := strings.Split(value, ",")
	for i := range values {
		values[i] = strings.TrimSpace(values[i])
	}
	return values
}

package config

import (
	"os"
	"testing"
)

func TestConfiguration(t *testing.T) {
	base := map[string]string{"DNSCALE_API_TOKEN_FILE": "/secret/token", "DNSCALE_DOMAIN_FILTER": "example.org", "DNSCALE_ZONE_ID_FILTER": "11111111-1111-4111-8111-111111111111", "DNSCALE_TXT_OWNER_ID": "cluster-example"}
	get := func(k string) string { return base[k] }
	read := func(string) ([]byte, error) { return []byte("test-token\n"), nil }
	c, err := load(get, read)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:8888" || c.Token != "test-token" || c.Provider.DefaultTTL != 300 || !c.Provider.DryRun {
		t.Fatal("incorrect defaults")
	}
	for _, tc := range []struct{ k, v string }{{"DNSCALE_DOMAIN_FILTER", ""}, {"DNSCALE_TXT_OWNER_ID", ""}, {"DNSCALE_ZONE_ID_FILTER", "invalid"}, {"DNSCALE_LISTEN", "0.0.0.0:8888"}, {"DNSCALE_API_URL", "http://api.example.org/v1"}, {"DNSCALE_API_URL", "https://user:password@api.example.org/v1"}, {"DNSCALE_DEFAULT_TTL", "60"}, {"DNSCALE_API_TIMEOUT", "0s"}, {"DNSCALE_DRY_RUN", "invalid"}} {
		old, exists := base[tc.k]
		base[tc.k] = tc.v
		if _, err := load(get, read); err == nil {
			t.Fatalf("accepted %s", tc.k)
		}
		if exists {
			base[tc.k] = old
		} else {
			delete(base, tc.k)
		}
	}
	if _, err := load(get, func(string) ([]byte, error) { return nil, os.ErrNotExist }); err == nil {
		t.Fatal("missing token accepted")
	}
}

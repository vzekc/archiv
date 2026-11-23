package archiv

import (
	"net/http/httptest"
	"testing"
)

func TestPublicHost(t *testing.T) {
	c := defaultConfig()
	c.SiteURL = "https://archiv.example.org"
	c.SiteAliases = []string{"alt.example.org"}
	for hdr, want := range map[string]string{
		"":                                 "archiv.example.org",
		"alt.example.org":                  "alt.example.org",
		"alt.example.org, alt.example.org": "alt.example.org",
		"Archiv.Example.org":               "archiv.example.org",
		"evil.example":                     "archiv.example.org",
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("X-Forwarded-Host", hdr)
		if got := c.publicHost(r); got != want {
			t.Errorf("%q: %q, want %q", hdr, got, want)
		}
	}
}

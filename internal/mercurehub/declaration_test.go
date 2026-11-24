package mercurehub

import (
	"strings"
	"testing"
)

func TestServes(t *testing.T) {
	for path, want := range map[string]bool{
		"/.well-known/mercure":                  true,
		"/.well-known/mercure/subscriptions":    true,
		"/.well-known/mercure/debug/":           true,
		MetadataPath:                            true,
		"/.well-known/mercurey":                 false,
		"/.well-known/acme-challenge/x":         false,
		"/.well-known/oauth-protected-resource": false,
		"/":                                     false,
		"/hello":                                false,
		"":                                      false,
	} {
		if got := Serves(path); got != want {
			t.Errorf("Serves(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestDeclarationValidate(t *testing.T) {
	timeouts := Declaration{Heartbeat: 40, WriteTimeout: 600, DispatchTimeout: 5}
	with := func(change func(*Declaration)) Declaration {
		d := timeouts
		change(&d)
		return d
	}
	key := Issuer{Identifier: "https://example.com", Subscriber: strings.Repeat("k", 32), Algorithm: "HS256"}
	for name, c := range map[string]struct {
		d    Declaration
		want string // "" for a declaration that is fine
	}{
		"anonymous":            {with(func(d *Declaration) { d.Anonymous = true }), ""},
		"nobody may subscribe": {timeouts, "issuer() or anonymous()"},
		"an issuer without a key": {with(func(d *Declaration) {
			d.Anonymous, d.Issuers = true, []Issuer{{Identifier: "https://example.com"}}
		}), "has no key"},
		"tokens for an unknown hub": {with(func(d *Declaration) { d.Issuers = []Issuer{key} }), "resource() or publicUrls()"},
		"tokens for a resource": {with(func(d *Declaration) {
			d.Issuers, d.Resource = []Issuer{key}, "https://example.com/.well-known/mercure"
		}), ""},
		"tokens for public URLs": {with(func(d *Declaration) {
			d.Issuers, d.PublicURLs = []Issuer{key}, []string{"https://example.com"}
		}), ""},
		"no dispatch timeout":  {with(func(d *Declaration) { d.Anonymous, d.DispatchTimeout = true, 0 }), "timeout"},
		"a negative heartbeat": {with(func(d *Declaration) { d.Anonymous, d.Heartbeat = true, -1 }), "timeout"},
	} {
		err := c.d.Validate()
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}

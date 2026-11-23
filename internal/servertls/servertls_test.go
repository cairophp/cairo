package servertls

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	for _, c := range []struct {
		name string
		decl Declaration
		want string // part of the error, "" for none
	}{
		{"files", Declaration{Cert: "c.pem", Key: "k.pem"}, ""},
		{"no key", Declaration{Cert: "c.pem"}, "a key file"},
		{"nothing", Declaration{}, "a certificate file"},
		{"domains", Declaration{Domains: []string{"example.com", "WWW.Example.com"}, CacheDir: "/var/certs"}, ""},
		{"no cache", Declaration{Domains: []string{"example.com"}}, "directory to keep"},
		{"wildcard", Declaration{Domains: []string{"*.example.com"}, CacheDir: "/c"}, "not a domain name"},
		{"address", Declaration{Domains: []string{"203.0.113.7"}, CacheDir: "/c"}, "not a domain name"},
		{"one label", Declaration{Domains: []string{"localhost"}, CacheDir: "/c"}, "not a domain name"},
		{"path in it", Declaration{Domains: []string{"example.com/x"}, CacheDir: "/c"}, "not a domain name"},
		{"too many", Declaration{Domains: make([]string, maxDomains+1), CacheDir: "/c"}, "at most"},
		{"plain directory", Declaration{Domains: []string{"example.com"}, CacheDir: "/c", Directory: "http://ca.test/dir"}, "https://"},
		{"other authority", Declaration{Domains: []string{"example.com"}, CacheDir: "/c", Directory: "https://ca.test/dir"}, ""},
	} {
		err := c.decl.Validate()
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: error %v, want one with %q", c.name, err, c.want)
		}
	}
}

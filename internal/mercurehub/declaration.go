// Package mercurehub is the Mercure hub of an HTTP server ($http->mercure()):
// github.com/dunglas/mercure, linked into the binary. That module is
// AGPL-3.0, and this package is the only one that imports it: the build tag
// nomercure leaves it out (nohub.go), and a declared hub is then an error.
package mercurehub

import (
	"fmt"
	"strings"
)

// Path is where the hub answers, as the Mercure protocol says. The hub also
// answers below it (the subscriptions) and at MetadataPath.
const (
	Path         = "/.well-known/mercure"
	MetadataPath = "/.well-known/oauth-protected-resource" + Path
)

// Declaration is $http->mercure().
type Declaration struct {
	Issuers []Issuer `json:"issuers"`
	// Anonymous lets clients without a token subscribe to public updates.
	Anonymous bool `json:"anonymous"`
	// Subscriptions turns on the subscription API and its events.
	Subscriptions  bool     `json:"subscriptions"`
	CORSOrigins    []string `json:"cors_origins"`
	PublishOrigins []string `json:"publish_origins"`
	// PublicURLs, when set, are the only origins the hub answers for.
	PublicURLs []string `json:"public_urls"`
	// Resource pins the audience of the tokens ("": the URL the client
	// asked, which is wrong behind a proxy that ends TLS).
	Resource string `json:"resource"`
	// History keeps the updates in a file, so that a client which comes
	// back gets what it missed ("": nothing is kept).
	History     string `json:"history"`
	HistorySize uint64 `json:"history_size"`
	// Compatibility is the version of the protocol before 1.0 whose clients
	// and tokens the hub also takes: 0 (none), 7 or 8.
	Compatibility int `json:"compatibility"`
	// Seconds; 0 turns the heartbeat or the write timeout off.
	Heartbeat       float64 `json:"heartbeat"`
	WriteTimeout    float64 `json:"write_timeout"`
	DispatchTimeout float64 `json:"dispatch_timeout"`
	// Limits of subscriptions that are open at once; 0 is no limit.
	MaxSubscriptions          int    `json:"max_subscriptions"`
	MaxSubscriptionsPerToken  int    `json:"max_subscriptions_per_token"`
	MaxSubscriptionsPerClient int    `json:"max_subscriptions_per_client"`
	CookieName                string `json:"cookie_name"`
	// Debugger serves the hub's debugging page below Path.
	Debugger bool `json:"debugger"`
}

// Issuer is who signs the tokens the hub trusts: the iss claim, and the key
// that verifies a publisher's and a subscriber's token ("" for a role the
// issuer has no tokens for).
type Issuer struct {
	Identifier string `json:"identifier"`
	Publisher  string `json:"publisher"`
	Subscriber string `json:"subscriber"`
	Algorithm  string `json:"algorithm"`
}

// Serves reports whether a request's path is the hub's.
func Serves(path string) bool {
	return strings.HasPrefix(path, "/.well-known/") &&
		(path == Path || path == MetadataPath || strings.HasPrefix(path, Path+"/"))
}

// Validate checks what needs no hub to check; Start checks the rest.
func (d *Declaration) Validate() error {
	if len(d.Issuers) == 0 && !d.Anonymous {
		return fmt.Errorf("mercure: a hub needs issuer() or anonymous(): nobody could subscribe")
	}
	for _, iss := range d.Issuers {
		if iss.Publisher == "" && iss.Subscriber == "" {
			return fmt.Errorf("mercure: issuer %q has no key", iss.Identifier)
		}
	}
	if len(d.Issuers) > 0 && d.Resource == "" && len(d.PublicURLs) == 0 {
		// The hub would take the audience that tokens must name from the
		// request's Host header, which is the client's to choose: a token
		// made for another hub of the same issuer would pass.
		return fmt.Errorf("mercure: with issuer(), say resource() or publicUrls(): the hub must know its own URL to check whom a token is for")
	}
	if d.Heartbeat < 0 || d.WriteTimeout < 0 || d.DispatchTimeout <= 0 {
		return fmt.Errorf("mercure: a timeout is negative, or the dispatch timeout is 0")
	}
	return nil
}

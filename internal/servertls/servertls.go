// Package servertls is the TLS side of a declared server: a certificate from
// two files, read again when they change, or certificates that an ACME
// certificate authority (Let's Encrypt) issues for the declared domains.
package servertls

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

// Declaration is a server's ->tls(...) or ->autoTls(...): the two files, or
// the domains.
type Declaration struct {
	// Cert and Key are PEM files: the certificate followed by its chain,
	// and its private key.
	Cert string `json:"cert"`
	Key  string `json:"key"`
	// Domains are the host names to get certificates for. CacheDir keeps
	// the account key and the certificates between restarts, Email is
	// where the authority writes about them, and Directory is the
	// authority's ACME directory ("": Let's Encrypt).
	Domains   []string `json:"domains"`
	CacheDir  string   `json:"cache_dir"`
	Email     string   `json:"email"`
	Directory string   `json:"directory"`
}

// maxDomains bounds the names one server asks certificates for.
const maxDomains = 100

// Validate checks the declaration before anything is served.
func (d *Declaration) Validate() error {
	if len(d.Domains) == 0 {
		if d.Cert == "" || d.Key == "" {
			return errors.New("tls needs a certificate file and a key file")
		}
		return nil
	}
	if d.CacheDir == "" {
		return errors.New("autoTls needs a directory to keep its certificates in")
	}
	if len(d.Domains) > maxDomains {
		return fmt.Errorf("autoTls takes at most %d domains", maxDomains)
	}
	for _, name := range d.Domains {
		if !validDomain(name) {
			return fmt.Errorf("autoTls: %q is not a domain name (an authority issues no certificate for an IP address or a wildcard here)", name)
		}
	}
	if d.Directory != "" && !strings.HasPrefix(d.Directory, "https://") {
		return fmt.Errorf("autoTls: the directory %q must be an https:// address", d.Directory)
	}
	return nil
}

// validDomain reports whether name is a host name of letters, digits and
// hyphens with at least two labels.
func validDomain(name string) bool {
	if len(name) > 253 || !strings.Contains(name, ".") {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range []byte(label) {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	last := name[strings.LastIndexByte(name, '.')+1:]
	return strings.Trim(last, "0123456789") != "" // not an IPv4 address
}

// TLS is what a server needs of its declaration.
type TLS struct {
	// Config is for the server's listener. TLS 1.2 is the oldest version
	// it accepts.
	Config  *tls.Config
	manager *autocert.Manager
}

// New reads the certificate files, or prepares the certificate manager. No
// certificate is asked for here: the manager gets one at the first handshake
// for a declared domain, and renews it thirty days before it ends.
func New(d Declaration, log *slog.Logger) (*TLS, error) {
	if len(d.Domains) == 0 {
		files, err := loadFiles(d.Cert, d.Key, log)
		if err != nil {
			return nil, err
		}
		return &TLS{Config: &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: files.certificate}}, nil
	}
	m := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		Cache:      autocert.DirCache(d.CacheDir),
		HostPolicy: autocert.HostWhitelist(d.Domains...),
		Email:      d.Email,
	}
	if d.Directory != "" {
		m.Client = &acme.Client{DirectoryURL: d.Directory}
	}
	cfg := m.TLSConfig()
	cfg.MinVersion = tls.VersionTLS12
	return &TLS{Config: cfg, manager: m}, nil
}

// HTTPHandler is for a plain HTTP listener beside the TLS one: with
// automatic certificates it answers the authority's HTTP challenges, and
// everything else goes to next.
func (t *TLS) HTTPHandler(next http.Handler) http.Handler {
	if t.manager == nil {
		return next
	}
	return t.manager.HTTPHandler(next)
}

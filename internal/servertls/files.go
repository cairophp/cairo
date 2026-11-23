package servertls

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// files is a certificate from two PEM files. Whatever renews it (certbot, a
// secret mounted by an orchestrator) replaces the files, so each handshake
// looks at them and loads them again when they changed: two stat calls, next
// to a handshake's signature.
type files struct {
	cert, key string
	log       *slog.Logger

	mu     sync.Mutex
	seen   [2]stamp
	loaded *tls.Certificate
}

// stamp is what tells one version of a file from the next.
type stamp struct {
	modified time.Time
	size     int64
}

func loadFiles(cert, key string, log *slog.Logger) (*files, error) {
	f := &files{cert: cert, key: key, log: log}
	pair, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		return nil, fmt.Errorf("tls certificate: %w", err)
	}
	f.loaded = &pair
	f.seen, _ = f.stamps()
	return f, nil
}

func (f *files) stamps() ([2]stamp, error) {
	var s [2]stamp
	for i, name := range [2]string{f.cert, f.key} {
		info, err := os.Stat(name)
		if err != nil {
			return s, err
		}
		s[i] = stamp{info.ModTime(), info.Size()}
	}
	return s, nil
}

// certificate is tls.Config.GetCertificate. A pair that does not load (one
// file replaced and the other not yet, a file gone) leaves the last good
// certificate in use.
func (f *files) certificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now, err := f.stamps()
	if err != nil || now == f.seen {
		return f.loaded, nil
	}
	f.seen = now
	pair, err := tls.LoadX509KeyPair(f.cert, f.key)
	if err != nil {
		f.log.Warn("tls certificate changed and does not load; keeping the one in use", "cert", f.cert, "err", err)
		return f.loaded, nil
	}
	f.loaded = &pair
	f.log.Info("tls certificate loaded again", "cert", f.cert)
	return f.loaded, nil
}

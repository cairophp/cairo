package sapi

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
)

// tlsOptions are the stream context's "ssl" options that Cairo honours.
type tlsOptions struct {
	Version         int    `json:"version"` // 10..13 for tlsv1.x://, 0 for any
	VerifyPeer      bool   `json:"verify_peer"`
	VerifyPeerName  bool   `json:"verify_peer_name"`
	AllowSelfSigned bool   `json:"allow_self_signed"`
	PeerName        string `json:"peer_name"`
	CAFile          string `json:"cafile"`
}

var tlsVersions = map[int]uint16{10: tls.VersionTLS10, 11: tls.VersionTLS11, 12: tls.VersionTLS12, 13: tls.VersionTLS13}

func (o tlsOptions) config(host string) (*tls.Config, error) {
	name := o.PeerName
	if name == "" {
		name = host
	}
	cfg := &tls.Config{ServerName: name}
	if v, ok := tlsVersions[o.Version]; ok {
		cfg.MinVersion, cfg.MaxVersion = v, v
	}
	if o.CAFile != "" {
		pem, err := os.ReadFile(o.CAFile)
		if err != nil {
			return nil, fmt.Errorf("cafile: %w", err)
		}
		cfg.RootCAs = x509.NewCertPool()
		if !cfg.RootCAs.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("cafile %s has no PEM certificates", o.CAFile)
		}
	}
	if !o.VerifyPeer {
		cfg.InsecureSkipVerify = true
		return cfg, nil
	}
	if o.VerifyPeerName && !o.AllowSelfSigned {
		return cfg, nil // Go's default verification: chain and name
	}
	// Verify by hand: the chain (unless the peer is self-signed and that is
	// allowed), and the name only when asked to.
	cfg.InsecureSkipVerify = true
	roots := cfg.RootCAs
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return errors.New("peer sent no certificate")
		}
		leaf := cs.PeerCertificates[0]
		if o.VerifyPeerName {
			if err := leaf.VerifyHostname(name); err != nil {
				return err
			}
		}
		if o.AllowSelfSigned && len(cs.PeerCertificates) == 1 && leaf.CheckSignatureFrom(leaf) == nil {
			return nil
		}
		inter := x509.NewCertPool()
		for _, c := range cs.PeerCertificates[1:] {
			inter.AddCert(c)
		}
		_, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter})
		return err
	}
	return cfg, nil
}

// dialTLS connects and handshakes with Go's crypto/tls, then hands PHP one
// end of a socket pair: Go copies plaintext between it and the TLS
// connection. PHP's stream code sees an ordinary blocking socket, so the
// socket hooks keep reads and writes fiber-friendly, and PHP needs no OpenSSL.
func dialTLS(ctx context.Context, address string, timeout time.Duration, o tlsOptions) (int, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return -1, err
	}
	cfg, err := o.config(host)
	if err != nil {
		return -1, err
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	d := tls.Dialer{Config: cfg}
	c, err := d.DialContext(ctx, "tcp", address)
	if err != nil {
		return -1, err
	}
	conn := c.(*tls.Conn)

	syscall.ForkLock.RLock()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err == nil {
		syscall.CloseOnExec(fds[0])
		syscall.CloseOnExec(fds[1])
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		conn.Close()
		return -1, err
	}
	f := os.NewFile(uintptr(fds[1]), "cairo-tls")
	local, err := net.FileConn(f)
	f.Close()
	if err != nil {
		syscall.Close(fds[0])
		conn.Close()
		return -1, err
	}
	go pumpTLS(conn, local.(*net.UnixConn))
	return fds[0], nil
}

// pumpTLS copies between PHP's socket and the TLS connection. When the server
// closes, PHP reads EOF; when PHP closes its stream, the TLS connection is
// closed with a close_notify.
func pumpTLS(conn *tls.Conn, local *net.UnixConn) {
	var once sync.Once
	closeAll := func() {
		once.Do(func() {
			_ = conn.Close()
			_ = local.Close()
		})
	}
	go func() {
		_, _ = io.Copy(local, conn)
		_ = local.CloseWrite() // PHP sees EOF; the stream stays open until PHP closes it
	}()
	_, _ = io.Copy(conn, local)
	closeAll()
}

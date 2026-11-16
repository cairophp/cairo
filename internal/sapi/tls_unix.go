package sapi

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
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
	CAPath          string `json:"capath"`
	LocalCert       string `json:"local_cert"` // PEM: certificate, and the key unless LocalPK is set
	LocalPK         string `json:"local_pk"`
	Passphrase      string `json:"passphrase"`
	SNIServerName   string `json:"SNI_server_name"`
}

var tlsVersions = map[int]uint16{10: tls.VersionTLS10, 11: tls.VersionTLS11, 12: tls.VersionTLS12, 13: tls.VersionTLS13}

func (o tlsOptions) config(host string) (*tls.Config, error) {
	name := o.PeerName
	if name == "" {
		name = host
	}
	cfg := &tls.Config{ServerName: name}
	if o.SNIServerName != "" {
		cfg.ServerName = o.SNIServerName
	}
	if v, ok := tlsVersions[o.Version]; ok {
		cfg.MinVersion, cfg.MaxVersion = v, v
	}
	if o.CAFile != "" || o.CAPath != "" {
		cfg.RootCAs = x509.NewCertPool()
		var files []string
		if o.CAFile != "" {
			files = append(files, o.CAFile)
		}
		if o.CAPath != "" {
			entries, err := os.ReadDir(o.CAPath)
			if err != nil {
				return nil, fmt.Errorf("capath: %w", err)
			}
			for _, e := range entries {
				if !e.IsDir() {
					files = append(files, filepath.Join(o.CAPath, e.Name()))
				}
			}
		}
		found := false
		for _, f := range files {
			pem, err := os.ReadFile(f)
			if err != nil {
				if f == o.CAFile {
					return nil, fmt.Errorf("cafile: %w", err)
				}
				continue
			}
			if cfg.RootCAs.AppendCertsFromPEM(pem) {
				found = true
			}
		}
		if !found {
			return nil, errors.New("cafile/capath hold no PEM certificates")
		}
	}
	if o.LocalCert != "" {
		cert, err := loadClientCert(o.LocalCert, o.LocalPK, o.Passphrase)
		if err != nil {
			return nil, err
		}
		cfg.Certificates = []tls.Certificate{cert}
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

// loadClientCert reads local_cert (and local_pk, or the key from the same
// file). An encrypted legacy PEM key ("Proc-Type: 4,ENCRYPTED") is
// decrypted with the passphrase.
func loadClientCert(certFile, keyFile, passphrase string) (tls.Certificate, error) {
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("local_cert: %w", err)
	}
	keyPEM := certPEM
	if keyFile != "" {
		if keyPEM, err = os.ReadFile(keyFile); err != nil {
			return tls.Certificate{}, fmt.Errorf("local_pk: %w", err)
		}
	}
	if passphrase != "" {
		var out []byte
		for rest := keyPEM; ; {
			var b *pem.Block
			b, rest = pem.Decode(rest)
			if b == nil {
				break
			}
			//lint:ignore SA1019 PHP's passphrase option is for these legacy encrypted keys
			if x509.IsEncryptedPEMBlock(b) { //nolint:staticcheck
				der, err := x509.DecryptPEMBlock(b, []byte(passphrase)) //nolint:staticcheck
				if err != nil {
					return tls.Certificate{}, fmt.Errorf("local_pk passphrase: %w", err)
				}
				b = &pem.Block{Type: b.Type, Bytes: der}
			}
			out = append(out, pem.EncodeToMemory(b)...)
		}
		keyPEM = out
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("local_cert: %w", err)
	}
	return cert, nil
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
	return tlsPair(c.(*tls.Conn))
}

// startTLS upgrades the connected socket fd in place (STARTTLS): Go
// handshakes over its own copy of the socket and returns a new descriptor
// for the plaintext side, which the caller puts in place of fd with dup2.
// On failure fd is left as it was.
func startTLS(ctx context.Context, fd int, host string, timeout time.Duration, o tlsOptions) (int, error) {
	cfg, err := o.config(host)
	if err != nil {
		return -1, err
	}
	syscall.ForkLock.RLock()
	nfd, err := syscall.Dup(fd)
	if err == nil {
		syscall.CloseOnExec(nfd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return -1, err
	}
	f := os.NewFile(uintptr(nfd), "cairo-starttls")
	raw, err := net.FileConn(f)
	f.Close()
	if err != nil {
		return -1, err
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	conn := tls.Client(raw, cfg)
	if err := conn.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		return -1, err
	}
	return tlsPair(conn)
}

// tlsPair hands a TLS connection to PHP as one end of a socket pair, with Go
// pumping between the other end and the connection.
func tlsPair(conn *tls.Conn) (int, error) {
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

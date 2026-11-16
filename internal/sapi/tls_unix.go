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
	"sync/atomic"
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

// tlsSession is one TLS connection handed to PHP through a socket pair. It
// is found again by PHP's end of the pair (device and inode, which survive
// the dup2 of STARTTLS) when PHP turns TLS off.
type tlsSession struct {
	key      tlsKey
	conn     *tls.Conn
	local    *net.UnixConn
	stopping atomic.Bool
	upDone   chan error // the PHP -> TLS pump handed over (nil) or failed
	downDone chan error // the TLS -> PHP pump ended (nil: close_notify or EOF)
}

type tlsKey struct{ dev, ino uint64 }

var tlsSessions sync.Map // tlsKey -> *tlsSession

func tlsKeyOf(fd int) (tlsKey, error) {
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return tlsKey{}, err
	}
	return tlsKey{uint64(st.Dev), uint64(st.Ino)}, nil
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
	key, err := tlsKeyOf(fds[0])
	if err != nil {
		syscall.Close(fds[0])
		syscall.Close(fds[1])
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
	s := &tlsSession{key: key, conn: conn, local: local.(*net.UnixConn), upDone: make(chan error, 1), downDone: make(chan error, 1)}
	tlsSessions.Store(key, s)
	go s.pump()
	return fds[0], nil
}

// pump copies between PHP's socket and the TLS connection. When the server
// closes, PHP reads EOF; when PHP closes its stream, the TLS connection is
// closed with a close_notify. When PHP turns TLS off (stop), the pumps hand
// over instead of closing.
func (s *tlsSession) pump() {
	go func() {
		_, err := io.Copy(s.local, s.conn)
		s.downDone <- err
		if !s.stopping.Load() {
			_ = s.local.CloseWrite() // PHP sees EOF; the stream stays open until PHP closes it
		}
	}()
	buf := make([]byte, 32*1024)
	for {
		n, err := s.local.Read(buf)
		if n > 0 {
			if _, werr := s.conn.Write(buf[:n]); werr != nil {
				err = werr
			}
		}
		if err != nil {
			if s.stopping.Load() && errors.Is(err, os.ErrDeadlineExceeded) {
				s.upDone <- s.drain(buf)
				return
			}
			break
		}
	}
	if !s.stopping.Load() {
		tlsSessions.Delete(s.key)
		_ = s.conn.Close()
		_ = s.local.Close()
	} else {
		s.upDone <- errors.New("the connection failed while TLS was being turned off")
	}
}

// drain sends what PHP wrote before turning TLS off and is still queued.
func (s *tlsSession) drain(buf []byte) error {
	_ = s.local.SetReadDeadline(time.Time{})
	raw, err := s.local.SyscallConn()
	if err != nil {
		return err
	}
	var werr error
	rerr := raw.Read(func(fd uintptr) bool {
		for {
			n, err := syscall.Read(int(fd), buf)
			if n > 0 {
				if _, werr = s.conn.Write(buf[:n]); werr != nil {
					return true
				}
			}
			if err != nil || n <= 0 {
				return true // EAGAIN: nothing more queued
			}
		}
	})
	if werr != nil {
		return werr
	}
	return rerr
}

// stopTLS turns TLS off on the stream whose descriptor is fd (PHP's end of a
// socket pair): what PHP wrote is sent, then a close_notify, and what the
// peer sent up to its own close_notify is passed to PHP's end. It returns a
// new descriptor for the plain connection, which the caller puts in place of
// fd with dup2 (after reading what is left on fd).
func stopTLS(ctx context.Context, fd int, timeout time.Duration) (int, error) {
	key, err := tlsKeyOf(fd)
	if err != nil {
		return -1, err
	}
	v, ok := tlsSessions.LoadAndDelete(key)
	if !ok {
		return -1, errors.New("TLS is not on for this stream")
	}
	s := v.(*tlsSession)
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	fail := func(err error) (int, error) {
		_ = s.conn.Close()
		_ = s.local.Close()
		return -1, err
	}

	s.stopping.Store(true)
	_ = s.local.SetReadDeadline(time.Now()) // stops the PHP -> TLS pump
	select {
	case err := <-s.upDone:
		if err != nil {
			return fail(err)
		}
	case <-ctx.Done():
		return fail(ctx.Err())
	}
	if err := s.conn.CloseWrite(); err != nil {
		return fail(err)
	}
	select {
	case err := <-s.downDone:
		if err != nil {
			return fail(fmt.Errorf("waiting for the peer's close_notify: %w", err))
		}
	case <-ctx.Done():
		return fail(fmt.Errorf("the peer did not answer the close_notify: %w", ctx.Err()))
	}

	raw, ok := s.conn.NetConn().(syscall.Conn)
	if !ok {
		return fail(errors.New("the connection has no descriptor"))
	}
	rc, err := raw.SyscallConn()
	if err != nil {
		return fail(err)
	}
	nfd := -1
	var derr error
	if err := rc.Control(func(fd uintptr) {
		syscall.ForkLock.RLock()
		nfd, derr = syscall.Dup(int(fd))
		if derr == nil {
			syscall.CloseOnExec(nfd)
		}
		syscall.ForkLock.RUnlock()
	}); err != nil {
		return fail(err)
	}
	if derr != nil {
		return fail(derr)
	}
	_ = s.conn.NetConn().Close() // Go's copy; PHP gets nfd
	_ = s.local.Close()
	return nfd, nil
}

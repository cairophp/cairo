package test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// testCert is a self-signed certificate for names and 127.0.0.1, as the two
// PEM texts a server reads.
func testCert(t *testing.T, serial int64, names ...string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(serial),
		Subject:               pkix.Name{CommonName: names[0]},
		DNSNames:              names,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// writeCert puts a certificate's two files in dir and returns their paths.
func writeCert(t *testing.T, dir string, certPEM, keyPEM []byte) (cert, key string) {
	t.Helper()
	cert, key = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	for name, text := range map[string][]byte{cert: certPEM, key: keyPEM} {
		// Written beside it and renamed, as a renewal does.
		if err := os.WriteFile(name+".new", text, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(name+".new", name); err != nil {
			t.Fatal(err)
		}
	}
	return cert, key
}

// trusting is a TLS client configuration that trusts exactly certPEM.
func trusting(t *testing.T, certPEM []byte) *tls.Config {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatal("certificate does not parse")
	}
	return &tls.Config{RootCAs: pool}
}

// startHTTPS serves examples/https on one PHP thread with env added, and
// returns the server's log.
func startHTTPS(t *testing.T, addr string, env ...string) *syncBuffer {
	t.Helper()
	logs := &syncBuffer{}
	cmd := exec.Command(cairoBin, "serve", "../examples/https/server.php", "--threads", "1", "--offload-threads", "1")
	cmd.Env = append(append(os.Environ(), "CAIRO_HTTP="+addr), env...)
	cmd.Stdout, cmd.Stderr = logs, logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("server log:\n%s", logs.String())
		}
	})
	if err := waitFor(addr, 10*time.Second); err != nil {
		t.Fatalf("%v\n%s", err, logs.String())
	}
	return logs
}

// fetch returns the answer's protocol and body.
func fetch(t *testing.T, client *http.Client, url string) (proto, body string) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d: %s", url, resp.StatusCode, b)
	}
	return resp.Proto, string(b)
}

// $http->tls(): the server answers over TLS with the files' certificate,
// with HTTP/2 for a client that asks for it and HTTP/1.1 for one that does
// not, and PHP knows which.
func TestHTTPSWithCertificateFiles(t *testing.T) {
	certPEM, keyPEM := testCert(t, 1, "localhost")
	cert, key := writeCert(t, t.TempDir(), certPEM, keyPEM)
	addr := freeAddr()
	startHTTPS(t, addr, "CAIRO_TLS_CERT="+cert, "CAIRO_TLS_KEY="+key)

	h2 := &http.Client{Transport: &http.Transport{TLSClientConfig: trusting(t, certPEM), ForceAttemptHTTP2: true}}
	if proto, body := fetch(t, h2, "https://"+addr+"/"); proto != "HTTP/2.0" || body != "HTTP/2.0 with TLS" {
		t.Errorf("HTTP/2 client: answer over %s says %q", proto, body)
	}
	h1cfg := trusting(t, certPEM)
	h1cfg.NextProtos = []string{"http/1.1"}
	h1 := &http.Client{Transport: &http.Transport{TLSClientConfig: h1cfg}}
	if proto, body := fetch(t, h1, "https://"+addr+"/"); proto != "HTTP/1.1" || body != "HTTP/1.1 with TLS" {
		t.Errorf("HTTP/1.1 client: answer over %s says %q", proto, body)
	}

	// TLS 1.1 is refused.
	old := trusting(t, certPEM)
	old.MinVersion, old.MaxVersion = tls.VersionTLS11, tls.VersionTLS11
	if conn, err := tls.Dial("tcp", addr, old); err == nil {
		conn.Close()
		t.Error("the server accepted TLS 1.1")
	}
	// A request without TLS on the TLS port gets no application answer.
	resp, err := http.Get("http://" + addr + "/")
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Error("the TLS port answered a plain HTTP request")
		}
	}
}

// One HTTP/2 connection carries 50 requests that each wait a second, on ONE
// PHP thread: they finish together.
func TestHTTP2StreamsShareOneThread(t *testing.T) {
	certPEM, keyPEM := testCert(t, 1, "localhost")
	cert, key := writeCert(t, t.TempDir(), certPEM, keyPEM)
	addr := freeAddr()
	startHTTPS(t, addr, "CAIRO_TLS_CERT="+cert, "CAIRO_TLS_KEY="+key)

	var dials int
	var mu sync.Mutex
	dialer := &tls.Dialer{Config: trusting(t, certPEM)}
	dialer.Config.NextProtos = []string{"h2"}
	client := &http.Client{Transport: &http.Transport{
		ForceAttemptHTTP2: true,
		DialTLSContext: func(ctx context.Context, network, a string) (net.Conn, error) {
			mu.Lock()
			dials++
			mu.Unlock()
			return dialer.DialContext(ctx, network, a)
		},
	}}
	fetch(t, client, "https://"+addr+"/") // the connection the 50 share

	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := client.Get("https://" + addr + "/wait")
			if err != nil {
				t.Error(err)
				return
			}
			defer resp.Body.Close()
			if b, _ := io.ReadAll(resp.Body); string(b) != "waited" || resp.Proto != "HTTP/2.0" {
				t.Errorf("answer %q over %s", b, resp.Proto)
			}
		}()
	}
	wg.Wait()
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("50 requests of 1 s took %v on one thread: they did not overlap", took)
	}
	if dials != 1 {
		t.Errorf("%d connections, want the one HTTP/2 connection", dials)
	}
}

// servedSerial is the serial number of the certificate the server presents.
func servedSerial(t *testing.T, addr, name string) (int64, error) {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: name, InsecureSkipVerify: true}) // the test reads the certificate itself
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	return conn.ConnectionState().PeerCertificates[0].SerialNumber.Int64(), nil
}

// A renewed certificate is served without a restart, and a pair of files
// that does not load leaves the last good one in use.
func TestCertificateFilesAreReadAgain(t *testing.T) {
	dir := t.TempDir()
	certPEM, keyPEM := testCert(t, 1, "localhost")
	cert, key := writeCert(t, dir, certPEM, keyPEM)
	addr := freeAddr()
	logs := startHTTPS(t, addr, "CAIRO_TLS_CERT="+cert, "CAIRO_TLS_KEY="+key)
	serial := func() int64 {
		n, err := servedSerial(t, addr, "localhost")
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := serial(); got != 1 {
		t.Fatalf("serial %d, want 1", got)
	}

	// A new certificate whose key file is not there yet: the pair does not match.
	cert2, key2 := testCert(t, 2, "localhost")
	if err := os.WriteFile(cert, cert2, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := serial(); got != 1 {
		t.Errorf("with a certificate and a key that do not match, serial %d, want the old 1", got)
	}
	// The server's log reaches the buffer through a pipe, after the handshake.
	waitUntil(t, "a warning about the pair that does not load", func() bool {
		return strings.Contains(logs.String(), "does not load")
	})
	if err := os.WriteFile(key, key2, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := serial(); got != 2 {
		t.Errorf("after the renewal, serial %d, want 2", got)
	}
}

// $http->h2c(): HTTP/2 without TLS for a client that starts with it, and
// HTTP/1.1 on the same port.
func TestHTTP2WithoutTLS(t *testing.T) {
	addr := freeAddr()
	startHTTPS(t, addr, "CAIRO_H2C=1")

	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	h2 := &http.Client{Transport: &http.Transport{Protocols: protocols}}
	if proto, body := fetch(t, h2, "http://"+addr+"/"); proto != "HTTP/2.0" || body != "HTTP/2.0 without TLS" {
		t.Errorf("HTTP/2 client: answer over %s says %q", proto, body)
	}
	if proto, body := fetch(t, &http.Client{Transport: &http.Transport{}}, "http://"+addr+"/"); proto != "HTTP/1.1" || body != "HTTP/1.1 without TLS" {
		t.Errorf("HTTP/1.1 client: answer over %s says %q", proto, body)
	}
}

// $http->redirectHttp(): the plain listener sends a request to the same
// host, path and query on the HTTPS server, and keeps the method.
func TestRedirectToHTTPS(t *testing.T) {
	certPEM, keyPEM := testCert(t, 1, "localhost")
	cert, key := writeCert(t, t.TempDir(), certPEM, keyPEM)
	addr, plain := freeAddr(), freeAddr()
	startHTTPS(t, addr, "CAIRO_TLS_CERT="+cert, "CAIRO_TLS_KEY="+key, "CAIRO_REDIRECT="+plain)
	if err := waitFor(plain, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(addr)
	_, plainPort, _ := net.SplitHostPort(plain)

	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest(http.MethodPost, "http://localhost:"+plainPort+"/orders/7?page=2", strings.NewReader("x"))
	resp, err := noFollow.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if want := "https://localhost:" + port + "/orders/7?page=2"; resp.StatusCode != http.StatusPermanentRedirect || resp.Header.Get("Location") != want {
		t.Errorf("status %d to %q, want 308 to %q", resp.StatusCode, resp.Header.Get("Location"), want)
	}

	follow := &http.Client{Transport: &http.Transport{TLSClientConfig: trusting(t, certPEM), ForceAttemptHTTP2: true}}
	if proto, body := fetch(t, follow, "http://localhost:"+plainPort+"/"); proto != "HTTP/2.0" || body != "HTTP/2.0 with TLS" {
		t.Errorf("following the redirect: answer over %s says %q", proto, body)
	}
}

// $http->autoTls(): the server presents the certificate it has for a
// declared domain, here one put in its cache as an earlier run would have
// left it, and refuses a name it was not told about. Nothing here asks a
// certificate authority for anything.
func TestAutomaticCertificates(t *testing.T) {
	cache := t.TempDir()
	certPEM, keyPEM := testCert(t, 7, "app.example.test")
	// autocert keeps a domain's key and certificates in one file named after it.
	if err := os.WriteFile(filepath.Join(cache, "app.example.test"), append(keyPEM, certPEM...), 0o600); err != nil {
		t.Fatal(err)
	}
	addr := freeAddr()
	// The directory is a closed port: a mistake that asks for a certificate fails at once.
	startHTTPS(t, addr, "CAIRO_TLS_DOMAINS=app.example.test", "CAIRO_TLS_CACHE="+cache, "CAIRO_ACME_DIRECTORY=https://127.0.0.1:1/directory")

	if got, err := servedSerial(t, addr, "app.example.test"); err != nil || got != 7 {
		t.Errorf("for the declared domain: serial %d, err %v; want the cached certificate 7", got, err)
	}
	if _, err := servedSerial(t, addr, "other.example.test"); err == nil {
		t.Error("the server completed a handshake for a domain that was not declared")
	}

	cfg := trusting(t, certPEM)
	cfg.ServerName = "app.example.test"
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg, ForceAttemptHTTP2: true}}
	if proto, body := fetch(t, client, "https://"+addr+"/"); proto != "HTTP/2.0" || body != "HTTP/2.0 with TLS" {
		t.Errorf("answer over %s says %q", proto, body)
	}
}

// $grpc->tls(): a gRPC client that verifies the certificate gets through.
func TestGRPCWithTLS(t *testing.T) {
	certPEM, keyPEM := testCert(t, 1, "localhost")
	cert, key := writeCert(t, t.TempDir(), certPEM, keyPEM)
	addr, grpcTLS := freeAddr(), freeAddr()
	startHTTPS(t, addr, "CAIRO_TLS_CERT="+cert, "CAIRO_TLS_KEY="+key, "CAIRO_GRPC="+grpcTLS)
	if err := waitFor(grpcTLS, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(grpcTLS)

	conn, err := grpc.NewClient("localhost:"+port, grpc.WithTransportCredentials(credentials.NewTLS(trusting(t, certPEM))))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("status %v", resp.Status)
	}
}

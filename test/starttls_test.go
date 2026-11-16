package test

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// startTLSServer speaks a small line protocol: a greeting, then STARTTLS,
// then ECHO over TLS. The handshake starts after delay. It reports the TLS
// version each client negotiated.
func startTLSServer(t *testing.T, cert tls.Certificate, delay time.Duration) (string, <-chan uint16) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	versions := make(chan uint16, 100)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				fmt.Fprint(c, "220 cairo test\r\n")
				r := bufio.NewReader(c)
				line, _ := r.ReadString('\n')
				if strings.TrimSpace(line) != "STARTTLS" {
					return
				}
				fmt.Fprint(c, "220 go ahead\r\n")
				time.Sleep(delay)
				tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
				if err := tc.Handshake(); err != nil {
					return
				}
				versions <- tc.ConnectionState().Version
				line, err := bufio.NewReader(tc).ReadString('\n')
				if err != nil {
					return
				}
				fmt.Fprintf(tc, "secure:%s\r\n", strings.TrimPrefix(strings.TrimSpace(line), "ECHO "))
				tc.Close()
			}(c)
		}
	}()
	return ln.Addr().String(), versions
}

type startTLSResult struct {
	Greeting string `json:"greeting"`
	Go       string `json:"go"`
	Enabled  any    `json:"enabled"`
	Echo     string `json:"echo"`
	Error    string `json:"error"`
}

func startTLSGet(t *testing.T, q url.Values) (startTLSResult, string) {
	raw := get(t, "/starttls?"+q.Encode())
	var out startTLSResult
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return out, strings.TrimSpace(raw)
}

func TestStartTLS(t *testing.T) {
	ca := newTestCA(t)
	cert, _, _ := ca.leaf(t, "starttls", x509.ExtKeyUsageServerAuth, net.ParseIP("127.0.0.1"))
	addr, versions := startTLSServer(t, cert, 0)
	cafile := filepath.Join(ca.dir, "ca.pem")

	for _, mode := range []string{"fiber", "offload"} {
		q := url.Values{"addr": {addr}, "cafile": {cafile}}
		if mode == "offload" {
			q.Set("offload", "1")
		}
		out, raw := startTLSGet(t, q)
		if out.Greeting != "220 cairo test" || out.Go != "220 go ahead" || out.Enabled != true || out.Echo != "secure:hello" {
			t.Fatalf("%s: got %s", mode, raw)
		}
		if v := <-versions; v != tls.VersionTLS13 {
			t.Fatalf("%s: negotiated %x, want TLS 1.3", mode, v)
		}
	}

	// The method picks the version.
	out, raw := startTLSGet(t, url.Values{"addr": {addr}, "cafile": {cafile}, "method": {"tls12"}})
	if out.Echo != "secure:hello" {
		t.Fatalf("tls12: got %s", raw)
	}
	if v := <-versions; v != tls.VersionTLS12 {
		t.Fatalf("tls12: negotiated %x", v)
	}

	// Without the CA the certificate is refused, with a warning.
	out, raw = startTLSGet(t, url.Values{"addr": {addr}})
	if out.Enabled != false || !strings.Contains(out.Error, "certificate") {
		t.Fatalf("unverified: got %s", raw)
	}
	// Server mode is not offered.
	out, raw = startTLSGet(t, url.Values{"addr": {addr}, "cafile": {cafile}, "method": {"server"}})
	if out.Enabled != false || !strings.Contains(out.Error, "client") {
		t.Fatalf("server method: got %s", raw)
	}
}

// 50 requests each upgrade a connection whose handshake the server delays by
// 1s; they overlap on one PHP thread.
func TestStartTLSDoesNotBlockTheThread(t *testing.T) {
	ca := newTestCA(t)
	cert, _, _ := ca.leaf(t, "starttls", x509.ExtKeyUsageServerAuth, net.ParseIP("127.0.0.1"))
	addr, _ := startTLSServer(t, cert, time.Second)
	q := url.Values{"addr": {addr}, "cafile": {filepath.Join(ca.dir, "ca.pem")}}.Encode()
	const n = 50
	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get("http://" + httpAddr + "/starttls?" + q)
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(b), `"echo":"secure:hello"`) {
				errs <- fmt.Errorf("status %d body %q", resp.StatusCode, b)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	took := time.Since(start)
	t.Logf("%d STARTTLS upgrades with a 1s handshake, on 1 PHP thread: %s", n, took.Round(time.Millisecond))
	if took > 3*time.Second {
		t.Fatalf("took %s; STARTTLS blocked the thread", took)
	}
}

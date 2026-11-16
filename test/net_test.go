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

	"golang.org/x/net/dns/dnsmessage"
)

// dnsServer answers cairo.test (A 10.1.2.3, TXT, MX) and NXDOMAIN for the rest.
func dnsServer(t *testing.T) string {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			var p dnsmessage.Parser
			h, err := p.Start(buf[:n])
			if err != nil {
				continue
			}
			q, err := p.Question()
			if err != nil {
				continue
			}
			b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: h.ID, Response: true, Authoritative: true})
			b.EnableCompression()
			_ = b.StartQuestions()
			_ = b.Question(q)
			_ = b.StartAnswers()
			rh := dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 60}
			if q.Name.String() != "cairo.test." {
				b = dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: h.ID, Response: true, Authoritative: true, RCode: dnsmessage.RCodeNameError})
				_ = b.StartQuestions()
				_ = b.Question(q)
			} else {
				switch q.Type {
				case dnsmessage.TypeA:
					_ = b.AResource(rh, dnsmessage.AResource{A: [4]byte{10, 1, 2, 3}})
				case dnsmessage.TypeTXT:
					_ = b.TXTResource(rh, dnsmessage.TXTResource{TXT: []string{"v=cairo"}})
				case dnsmessage.TypeMX:
					_ = b.MXResource(rh, dnsmessage.MXResource{Pref: 10, MX: dnsmessage.MustNewName("mail.cairo.test.")})
				}
			}
			msg, err := b.Finish()
			if err == nil {
				_, _ = pc.WriteTo(msg, from)
			}
		}
	}()
	return pc.LocalAddr().String()
}

// lineServer sends "hello\n" to each connection after delay.
func lineServer(t *testing.T, delay time.Duration) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				time.Sleep(delay)
				fmt.Fprint(c, "hello\n")
				_, _ = io.Copy(io.Discard, c)
			}()
		}
	}()
	return ln.Addr().String()
}

func TestNetEcho(t *testing.T) {
	raw := get(t, "/net/echo")
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	want := map[string]any{
		"first": "echo: one\n", "second": "echo: two\n", "third": "echo: three\n", "after_close": nil,
		"lines": float64(3), "remote_is_listener": true, "timeout": "timeout", "accept_timeout": "timeout", "refused": "dial",
	}
	for k, v := range want {
		if out[k] != v {
			t.Fatalf("%s = %#v, want %#v; got %s", k, out[k], v, strings.TrimSpace(raw))
		}
	}
}

func TestNetTLSUDPAndDNS(t *testing.T) {
	ca := newTestCA(t)
	cert, _, _ := ca.leaf(t, "net", x509.ExtKeyUsageServerAuth, net.ParseIP("127.0.0.1"))
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				line, _ := bufio.NewReader(c).ReadString('\n')
				fmt.Fprintf(c, "tls %s", line)
			}()
		}
	}()
	q := url.Values{
		"addr":   {ln.Addr().String()},
		"cafile": {filepath.Join(ca.dir, "ca.pem")},
		"dns":    {dnsServer(t)},
		"plain":  {lineServer(t, 0)},
	}
	raw := get(t, "/net/more?"+q.Encode())
	var out struct {
		TLS           string `json:"tls"`
		TLSUnverified string `json:"tls_unverified"`
		UDP           struct {
			Data  string `json:"data"`
			FromA bool   `json:"from_a"`
		} `json:"udp"`
		DNS struct {
			Host    []string         `json:"host"`
			TXT     []string         `json:"txt"`
			MX      []map[string]any `json:"mx"`
			Missing string           `json:"missing"`
		} `json:"dns"`
		Localhost bool   `json:"localhost"`
		Offload   string `json:"offload"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	fail := func(why string) { t.Helper(); t.Fatalf("%s: got %s", why, strings.TrimSpace(raw)) }
	if out.TLS != "tls ping\n" || out.TLSUnverified != "tls" {
		fail("tls")
	}
	if out.UDP.Data != "datagram" || !out.UDP.FromA {
		fail("udp")
	}
	if strings.Join(out.DNS.Host, ",") != "10.1.2.3" || strings.Join(out.DNS.TXT, ",") != "v=cairo" ||
		len(out.DNS.MX) != 1 || out.DNS.MX[0]["host"] != "mail.cairo.test." || out.DNS.Missing != "not_found" {
		fail("dns")
	}
	if !out.Localhost {
		fail("system resolver")
	}
	if out.Offload != "hello\n" {
		fail("outside a fiber")
	}
}

// 50 requests each wait 1s for a line from a server; they overlap on one PHP
// thread.
func TestNetDoesNotBlockTheThread(t *testing.T) {
	addr := lineServer(t, time.Second)
	const n = 50
	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get("http://" + httpAddr + "/net/line?addr=" + url.QueryEscape(addr))
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(b), `"line":"hello\n"`) {
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
	t.Logf("%d Cairo\\Net reads waiting 1s, on 1 PHP thread: %s", n, took.Round(time.Millisecond))
	if took > 3*time.Second {
		t.Fatalf("took %s; Cairo\\Net blocked the thread", took)
	}
}

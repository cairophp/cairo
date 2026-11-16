package test

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cccServer speaks a line protocol that turns TLS on and off, as FTP's CCC
// does: STARTTLS, ECHO over TLS, CCC (reply, one more secure line, then the
// TLS shutdown), PLAIN in clear text, and STARTTLS again.
func cccServer(t *testing.T, cert tls.Certificate) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	cfg := &tls.Config{Certificates: []tls.Certificate{cert}}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				fmt.Fprint(c, "220 ccc test\r\n")
				var rw io.ReadWriter = c
				r := bufio.NewReader(rw)
				var tc *tls.Conn
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					cmd, arg, _ := strings.Cut(strings.TrimSpace(line), " ")
					switch cmd {
					case "STARTTLS":
						fmt.Fprint(rw, "220 go ahead\r\n")
						tc = tls.Server(c, cfg)
						if tc.Handshake() != nil {
							return
						}
						rw, r = tc, bufio.NewReader(tc)
					case "ECHO":
						fmt.Fprintf(rw, "secure:%s\r\n", arg)
					case "CCC":
						fmt.Fprint(rw, "200 clear\r\nlast secure line\r\n")
						// Wait for the client's close_notify, answer with ours.
						if _, err := io.Copy(io.Discard, r); err != nil && !errors.Is(err, io.EOF) {
							return
						}
						if tc.CloseWrite() != nil {
							return
						}
						_ = c.SetWriteDeadline(time.Time{}) // CloseWrite leaves one in the past
						rw, r, tc = c, bufio.NewReader(c), nil
					case "PLAIN":
						if tc != nil {
							fmt.Fprint(rw, "still secure\r\n")
						} else {
							fmt.Fprintf(rw, "plain:%s\r\n", arg)
						}
					default:
						return
					}
				}
			}(c)
		}
	}()
	return ln.Addr().String()
}

// stream_socket_enable_crypto($s, false) in a request fiber and on an
// offload thread: the close_notify exchange, a line the server sent before
// its close_notify, clear text afterwards, and TLS on again.
func TestTLSOff(t *testing.T) {
	ca := newTestCA(t)
	cert, _, _ := ca.leaf(t, "ccc", x509.ExtKeyUsageServerAuth, net.ParseIP("127.0.0.1"))
	addr := cccServer(t, cert)
	for _, mode := range []string{"fiber", "offload"} {
		q := url.Values{"addr": {addr}, "cafile": {filepath.Join(ca.dir, "ca.pem")}}
		if mode == "offload" {
			q.Set("offload", "1")
		}
		raw := get(t, "/tls-off?"+q.Encode())
		var out map[string]any
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatalf("%s: %s: %v", mode, raw, err)
		}
		want := map[string]any{
			"greeting": "220 ccc test", "on": true, "secure": "secure:one", "ccc": "200 clear", "off": true,
			"last_secure": "last secure line", "plain": "plain:two", "on_again": true, "secure_again": "secure:three",
			"not_on": false,
		}
		for k, v := range want {
			if out[k] != v {
				t.Errorf("%s: %s = %#v, want %#v (%s)", mode, k, out[k], v, raw)
			}
		}
		if e, _ := out["not_on_error"].(string); !strings.Contains(e, "TLS is not on") {
			t.Errorf("%s: turning TLS off on a plain stream: %q", mode, e)
		}
	}
}

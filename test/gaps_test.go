package test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testCA makes a CA and signs leaf certificates with it, written as PEM files.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	dir  string
}

func newTestCA(t *testing.T) *testCA {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "cairo test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	ca := &testCA{cert: cert, key: key, dir: t.TempDir()}
	ca.write(t, "ca.pem", "CERTIFICATE", der)
	return ca
}

func (ca *testCA) write(t *testing.T, name, typ string, der []byte) string {
	p := filepath.Join(ca.dir, name)
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// leaf returns a tls.Certificate plus its cert and key file paths.
func (ca *testCA) leaf(t *testing.T, name string, usage x509.ExtKeyUsage, ips ...net.IP) (tls.Certificate, string, string) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	certFile := ca.write(t, name+".pem", "CERTIFICATE", der)
	keyFile := ca.write(t, name+"-key.pem", "EC PRIVATE KEY", keyDER)
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	return pair, certFile, keyFile
}

// Mutual TLS: a server that requires a client certificate accepts PHP's
// local_cert/local_pk and refuses a connection without one.
func TestTLSClientCertificate(t *testing.T) {
	ca := newTestCA(t)
	serverCert, _, _ := ca.leaf(t, "server", x509.ExtKeyUsageServerAuth, net.ParseIP("127.0.0.1"))
	_, clientCert, clientKey := ca.leaf(t, "php-client", x509.ExtKeyUsageClientAuth)
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello "+r.TLS.PeerCertificates[0].Subject.CommonName)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{serverCert}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert}
	srv.Config.ErrorLog = nil
	srv.StartTLS()
	defer srv.Close()

	call := func(withCert bool) (body *string, errText string) {
		q := "/tls/client-cert?url=" + url.QueryEscape(srv.URL+"/") + "&cafile=" + url.QueryEscape(filepath.Join(ca.dir, "ca.pem"))
		if withCert {
			q += "&cert=" + url.QueryEscape(clientCert) + "&key=" + url.QueryEscape(clientKey)
		}
		var out struct {
			Body  *string `json:"body"`
			Error string  `json:"error"`
		}
		raw := get(t, q)
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		return out.Body, out.Error
	}
	if body, errText := call(true); body == nil || *body != "hello php-client" {
		t.Fatalf("with a client certificate: body %v, error %q", body, errText)
	}
	if body, _ := call(false); body != nil {
		t.Fatalf("a server requiring a client certificate answered without one: %q", *body)
	}
}

func TestPDOPgsqlByteaIsAStream(t *testing.T) {
	pgsqlEnv(t)
	raw := get(t, "/pgsql/bytea")
	var out struct {
		IsStream    bool    `json:"is_stream"`
		Hex         string  `json:"hex"`
		Null        *string `json:"null"`
		BoundString string  `json:"bound_string"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	if !out.IsStream || out.Hex != "00ff62696e" || out.Null != nil || out.BoundString != "00ff62696e" {
		t.Fatalf("got %s", strings.TrimSpace(raw))
	}
}

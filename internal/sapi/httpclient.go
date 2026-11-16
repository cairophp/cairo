package sapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Cairo\Http\Client sends its requests through Go's net/http: connection
// pooling, keep-alive, HTTP/2 over TLS and proxies from the environment come
// with it. Inside a request fiber the request runs on the fiber's goroutine,
// so the fiber suspends and the request's deadline cancels it.

func init() {
	registerOp("http", func(ctx context.Context, payload []byte) (any, error) {
		var req httpClientRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		return httpDo(ctx, req), nil
	})
}

type httpClientRequest struct {
	Method    string              `json:"method"`
	URL       string              `json:"url"`
	Headers   map[string][]string `json:"headers"`
	Body      string              `json:"body"`      // base64
	Timeout   float64             `json:"timeout"`   // seconds, 0 for none
	Redirects int                 `json:"redirects"` // how many to follow
	TLS       httpClientTLS       `json:"tls"`
}

type httpClientTLS struct {
	Verify bool   `json:"verify"`
	CAFile string `json:"cafile"`
	Cert   string `json:"cert"`
	Key    string `json:"key"`
}

var (
	httpTransportsMu sync.Mutex
	httpTransports   = map[httpClientTLS]*http.Transport{}
)

// httpTransport returns the shared transport (and so connection pool) for
// one set of TLS settings.
func httpTransport(o httpClientTLS) (*http.Transport, error) {
	httpTransportsMu.Lock()
	defer httpTransportsMu.Unlock()
	if t, ok := httpTransports[o]; ok {
		return t, nil
	}
	cfg := &tls.Config{InsecureSkipVerify: !o.Verify}
	if o.CAFile != "" {
		pem, err := os.ReadFile(o.CAFile)
		if err != nil {
			return nil, fmt.Errorf("cafile: %w", err)
		}
		cfg.RootCAs = x509.NewCertPool()
		if !cfg.RootCAs.AppendCertsFromPEM(pem) {
			return nil, errors.New("cafile holds no PEM certificates")
		}
	}
	if o.Cert != "" {
		key := o.Key
		if key == "" {
			key = o.Cert
		}
		cert, err := tls.LoadX509KeyPair(o.Cert, key)
		if err != nil {
			return nil, fmt.Errorf("client certificate: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = cfg
	t.MaxIdleConnsPerHost = 64
	httpTransports[o] = t
	return t, nil
}

// httpDo sends one request. The result has "ok"; a response carries status,
// headers, the body (base64), the final URL and the protocol; a failure
// carries "error" and "kind" (timeout, tls, connect, redirects, request).
func httpDo(ctx context.Context, req httpClientRequest) map[string]any {
	fail := func(kind string, err error) map[string]any {
		return map[string]any{"ok": false, "kind": kind, "error": err.Error()}
	}
	body, err := base64.StdEncoding.DecodeString(req.Body)
	if err != nil {
		return fail("request", errors.New("bad body encoding"))
	}
	transport, err := httpTransport(req.TLS)
	if err != nil {
		return fail("tls", err)
	}
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.Timeout*float64(time.Second)))
		defer cancel()
	}
	method := strings.ToUpper(req.Method)
	if method == "" {
		method = http.MethodGet
	}
	var r io.Reader
	if len(body) > 0 {
		r = bytes.NewReader(body)
	}
	hreq, err := http.NewRequestWithContext(ctx, method, req.URL, r)
	if err != nil {
		return fail("request", err)
	}
	for k, vs := range req.Headers {
		for _, v := range vs {
			hreq.Header.Add(k, v)
		}
		if strings.EqualFold(k, "Host") && len(vs) > 0 {
			hreq.Host = vs[0]
		}
	}
	errRedirects := errors.New("too many redirects")
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) > req.Redirects {
				if req.Redirects == 0 {
					return http.ErrUseLastResponse
				}
				return fmt.Errorf("stopped after %d redirects: %w", req.Redirects, errRedirects)
			}
			return nil
		},
	}
	resp, err := client.Do(hreq)
	if err == nil {
		var data []byte
		data, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		if err == nil {
			headers := map[string][]string(resp.Header)
			if headers == nil {
				headers = map[string][]string{}
			}
			return map[string]any{
				"ok":       true,
				"status":   resp.StatusCode,
				"reason":   strings.TrimSpace(strings.TrimPrefix(resp.Status, fmt.Sprint(resp.StatusCode))),
				"headers":  headers,
				"body":     base64.StdEncoding.EncodeToString(data),
				"url":      resp.Request.URL.String(),
				"protocol": resp.Proto,
			}
		}
	}
	var netErr net.Error
	var tlsErr *tls.CertificateVerificationError
	var recErr tls.RecordHeaderError
	switch {
	case ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fail("timeout", err)
	case errors.Is(err, errRedirects):
		return fail("redirects", err)
	case errors.As(err, &tlsErr), errors.As(err, &recErr), strings.Contains(err.Error(), "tls: "):
		return fail("tls", err)
	case errors.As(err, &netErr) && netErr.Timeout():
		return fail("timeout", err)
	default:
		return fail("connect", err)
	}
}

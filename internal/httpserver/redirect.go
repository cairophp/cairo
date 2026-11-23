package httpserver

import (
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/cairophp/cairo/internal/servertls"
)

// redirect is $http->redirectHttp(): a plain HTTP listener that sends every
// request to the same host and path on the HTTPS server. With automatic
// certificates it also answers the authority's HTTP challenges.
type redirect struct {
	srv *http.Server
	ln  net.Listener
}

func newRedirect(listen string, https net.Addr, secure *servertls.TLS, errorLog *log.Logger) (*redirect, error) {
	_, port, err := net.SplitHostPort(https.String())
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	return &redirect{
		srv: &http.Server{
			Handler: secure.HTTPHandler(toHTTPS(port)),
			// A redirect needs the request line and the headers only.
			ReadHeaderTimeout: 10 * time.Second,
			ErrorLog:          errorLog,
		},
		ln: ln,
	}, nil
}

func (r *redirect) serve() error { return r.srv.Serve(r.ln) }

// toHTTPS answers with the request's own address on HTTPS, at port unless
// that is 443. 308 keeps the method and the body.
func toHTTPS(port string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		host, _, err := net.SplitHostPort(req.Host)
		if err != nil {
			host = strings.Trim(req.Host, "[]") // no port in it
		}
		if host == "" {
			http.Error(w, "missing Host header", http.StatusBadRequest)
			return
		}
		switch {
		case port != "443":
			host = net.JoinHostPort(host, port)
		case strings.Contains(host, ":"):
			host = "[" + host + "]" // an IPv6 address
		}
		http.Redirect(w, req, "https://"+host+req.URL.RequestURI(), http.StatusPermanentRedirect)
	})
}

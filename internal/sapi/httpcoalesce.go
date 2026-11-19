package sapi

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Cairo\Http\Client with 'coalesce' => true: identical GET and HEAD requests
// in flight at the same moment, from any fiber on any thread, share one
// upstream call. Identical means the same method, URL, headers (so the same
// Authorization and Cookie), TLS settings, redirects and timeout, so two
// callers only share a call that would have been the same request.
//
// The shared call runs on its own context: a caller that leaves (cancelled,
// past its deadline) stops waiting without failing the others, and the call
// is cancelled when no caller is left.

type httpFlight struct {
	done    chan struct{}
	res     map[string]any
	waiters int // guarded by httpFlightsMu
	cancel  context.CancelFunc
}

var (
	httpFlightsMu sync.Mutex
	httpFlights   = map[string]*httpFlight{}
)

// httpCanCoalesce reports whether req may share a call with others.
func httpCanCoalesce(req httpClientRequest) bool {
	m := strings.ToUpper(req.Method)
	return req.Body == "" && (m == "" || m == http.MethodGet || m == http.MethodHead)
}

func httpCoalesced(ctx context.Context, req httpClientRequest) map[string]any {
	key := httpFlightKey(req)
	httpFlightsMu.Lock()
	f, ok := httpFlights[key]
	if !ok {
		fctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		f = &httpFlight{done: make(chan struct{}), cancel: cancel}
		httpFlights[key] = f
		go func() {
			defer cancel()
			res := httpDo(fctx, req)
			httpFlightsMu.Lock()
			if httpFlights[key] == f {
				delete(httpFlights, key)
			}
			httpFlightsMu.Unlock()
			f.res = res
			close(f.done)
		}()
	}
	f.waiters++
	httpFlightsMu.Unlock()

	select {
	case <-f.done:
		return f.res
	case <-ctx.Done():
	}
	httpFlightsMu.Lock()
	if f.waiters--; f.waiters == 0 {
		// Nobody waits any more: stop the call, and let the next caller
		// start a new one rather than join a cancelled one.
		if httpFlights[key] == f {
			delete(httpFlights, key)
		}
		f.cancel()
	}
	httpFlightsMu.Unlock()
	kind := "connect"
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		kind = "timeout"
	}
	return map[string]any{"ok": false, "kind": kind, "error": ctx.Err().Error()}
}

// httpFlightKey is everything that makes two requests the same request.
// Each part is length-prefixed, so no two different requests share a key.
func httpFlightKey(req httpClientRequest) string {
	names := make([]string, 0, len(req.Headers))
	for name := range req.Headers {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	put := func(s string) {
		b.WriteString(strconv.Itoa(len(s)))
		b.WriteByte(':')
		b.WriteString(s)
	}
	for _, s := range []string{strings.ToUpper(req.Method), req.URL, strconv.Itoa(req.Redirects),
		strconv.FormatFloat(req.Timeout, 'g', -1, 64), strconv.FormatBool(req.TLS.Verify),
		req.TLS.CAFile, req.TLS.Cert, req.TLS.Key} {
		put(s)
	}
	for _, name := range names {
		put(http.CanonicalHeaderKey(name))
		b.WriteString(strconv.Itoa(len(req.Headers[name])))
		b.WriteByte('#')
		for _, v := range req.Headers[name] {
			put(v)
		}
	}
	return b.String()
}

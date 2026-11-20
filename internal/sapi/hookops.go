package sapi

/*
#include "hooks.h"
*/
import "C"

import (
	"context"
	"encoding/json"
	"time"
	"unsafe"

	"github.com/cairophp/cairo/internal/core"
)

// What PHP's hooked functions (connect, TLS on and off, gethostbyname) ask Go
// for. Inside a fiber they are fiber operations, run on the fiber's goroutine
// so the request's deadline ends them; outside one (or with hooks off) the
// PHP thread calls goDialTLS, goStartTLS and goStopTLS below and waits.

func init() {
	registerPHP("hooks.php")
	registerC(&C.cairo_function_hooks)
	registerC(&C.cairo_socket_hooks)
	core.RegisterFiberOp("dial", opDial)
	core.RegisterFiberOp("starttls", opStartTLS)
	core.RegisterFiberOp("stoptls", opStopTLS)
	core.RegisterFiberOp("lookup", opLookup)
}

// opDial is a hooked connect: DNS, the connect and, for TLS, the handshake.
func opDial(ctx context.Context, payload []byte) (any, string) {
	var req struct {
		Network string      `json:"network"`
		Address string      `json:"address"`
		Timeout float64     `json:"timeout"`
		TLS     *tlsOptions `json:"tls"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, err.Error()
	}
	timeout := hookTimeout(req.Timeout)
	var fd int
	var err error
	if req.Network == "tls" && req.TLS != nil {
		fd, err = dialTLS(ctx, req.Address, timeout, *req.TLS)
	} else {
		fd, err = dialFD(ctx, req.Network, req.Address, timeout)
	}
	return fdResult(ctx, fd, err)
}

// opStartTLS is stream_socket_enable_crypto(): the handshake.
func opStartTLS(ctx context.Context, payload []byte) (any, string) {
	var req struct {
		Fd      int         `json:"fd"`
		Host    string      `json:"host"`
		Timeout float64     `json:"timeout"`
		TLS     *tlsOptions `json:"tls"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, err.Error()
	}
	timeout := hookTimeout(req.Timeout)
	var opts tlsOptions
	if req.TLS != nil {
		opts = *req.TLS
	}
	fd, err := startTLS(ctx, req.Fd, req.Host, timeout, opts)
	return fdResult(ctx, fd, err)
}

// opStopTLS is stream_socket_enable_crypto($s, false): the TLS shutdown.
func opStopTLS(ctx context.Context, payload []byte) (any, string) {
	var req struct {
		Fd      int     `json:"fd"`
		Timeout float64 `json:"timeout"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, err.Error()
	}
	timeout := hookTimeout(req.Timeout)
	fd, err := stopTLS(ctx, req.Fd, timeout)
	return fdResult(ctx, fd, err)
}

// hookTimeout is a hook's timeout in seconds as a duration; none (<= 0) is 0.
func hookTimeout(seconds float64) time.Duration {
	if seconds > 0 {
		return time.Duration(seconds * float64(time.Second))
	}
	return 0
}

// fdResult is what a connect or TLS operation wakes its fiber with: the new
// descriptor, even when the context ended meanwhile (PHP owns it from here);
// the context's error; or the failure as a message for the stream's own
// error reporting.
func fdResult(ctx context.Context, fd int, err error) (any, string) {
	switch {
	case err == nil:
		return map[string]any{"fd": fd}, ""
	case ctx.Err() != nil:
		return nil, core.CtxError(ctx)
	default:
		return map[string]any{"err": err.Error()}, ""
	}
}

// opLookup is the hooked gethostbyname(): an unknown name is an empty list.
func opLookup(ctx context.Context, payload []byte) (any, string) {
	var req struct {
		Host string `json:"host"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, err.Error()
	}
	ips, err := lookupIPv4(ctx, req.Host)
	switch {
	case err == nil:
		return ips, ""
	case ctx.Err() != nil:
		return nil, core.CtxError(ctx)
	default:
		return []string{}, ""
	}
}

// goDialTLS connects a TLS stream for code outside a Cairo fiber (or with
// hooks off): the PHP thread waits here. Returns the descriptor, or -1 with
// *errOut set (the caller frees it).
//
//export goDialTLS
func goDialTLS(data *C.char, n C.int, errOut **C.char) C.int {
	var req struct {
		Address string     `json:"address"`
		Timeout float64    `json:"timeout"`
		TLS     tlsOptions `json:"tls"`
	}
	if err := json.Unmarshal(C.GoBytes(unsafe.Pointer(data), n), &req); err != nil {
		*errOut = C.CString(err.Error())
		return -1
	}
	timeout := hookTimeout(req.Timeout)
	fd, err := dialTLS(context.Background(), req.Address, timeout, req.TLS)
	if err != nil {
		*errOut = C.CString(err.Error())
		return -1
	}
	return C.int(fd)
}

// goStartTLS upgrades socket fd for code outside a Cairo fiber: the PHP
// thread waits here. Returns the new descriptor, or -1 with *errOut set (the
// caller frees it).
//
//export goStartTLS
func goStartTLS(fd C.int, data *C.char, n C.int, errOut **C.char) C.int {
	var req struct {
		Host    string     `json:"host"`
		Timeout float64    `json:"timeout"`
		TLS     tlsOptions `json:"tls"`
	}
	if err := json.Unmarshal(C.GoBytes(unsafe.Pointer(data), n), &req); err != nil {
		*errOut = C.CString(err.Error())
		return -1
	}
	timeout := hookTimeout(req.Timeout)
	nfd, err := startTLS(context.Background(), int(fd), req.Host, timeout, req.TLS)
	if err != nil {
		*errOut = C.CString(err.Error())
		return -1
	}
	return C.int(nfd)
}

// goStopTLS turns TLS off on socket fd for code outside a Cairo fiber: the
// PHP thread waits here. Returns the plain connection's descriptor, or -1
// with *errOut set (the caller frees it).
//
//export goStopTLS
func goStopTLS(fd C.int, timeoutSec C.double, errOut **C.char) C.int {
	timeout := hookTimeout(float64(timeoutSec))
	nfd, err := stopTLS(context.Background(), int(fd), timeout)
	if err != nil {
		*errOut = C.CString(err.Error())
		return -1
	}
	return C.int(nfd)
}

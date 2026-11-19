package sapi

import "C"

import (
	"context"
	"encoding/json"
	"time"
	"unsafe"
)

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
	timeout := time.Duration(0)
	if req.Timeout > 0 {
		timeout = time.Duration(req.Timeout * float64(time.Second))
	}
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
	timeout := time.Duration(0)
	if req.Timeout > 0 {
		timeout = time.Duration(req.Timeout * float64(time.Second))
	}
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
	timeout := time.Duration(0)
	if timeoutSec > 0 {
		timeout = time.Duration(float64(timeoutSec) * float64(time.Second))
	}
	nfd, err := stopTLS(context.Background(), int(fd), timeout)
	if err != nil {
		*errOut = C.CString(err.Error())
		return -1
	}
	return C.int(nfd)
}

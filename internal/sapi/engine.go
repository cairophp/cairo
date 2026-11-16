// Package sapi embeds a thread-safe (ZTS) PHP into the Go process and moves
// work between Go and PHP threads.
//
// The model, in one paragraph: every PHP Fiber has exactly one Go goroutine.
// A request's root fiber belongs to the goroutine that accepted the request
// (Engine.Dispatch); fibers started with Cairo\async() get a child goroutine.
// That goroutine owns the fiber's context.Context and performs every I/O
// operation the fiber asks for, then wakes the fiber on its PHP thread. So
// one PHP thread can hold thousands of requests, each waiting on its own
// goroutine. CPU-heavy work goes to a separate pool of PHP threads through
// Cairo\offload(), so it never stalls the fibers on a request thread.
package sapi

/*
#cgo CFLAGS: -D_GNU_SOURCE
#cgo LDFLAGS: -lphp -lcurl
#include <stdlib.h>
#include "cairo.h"
#include "pgsql.h"
*/
import "C"

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/mohamed-samir907/cairo/internal/spec"
	"github.com/mohamed-samir907/cairo/php"
)

// inboxSize bounds how many events can queue for one PHP thread before the
// sender waits. It does not limit requests in flight.
const inboxSize = 4096

// Engine is the embedded PHP runtime. There is one per process: PHP's
// process-wide state cannot be initialised twice.
type Engine struct {
	serverFile  string
	runtimeFile string
	log         *slog.Logger
	phpIni      string

	threads []*thread // all PHP threads; index = thread id
	workers []*thread // run requests as fibers
	offload []*thread // run Cairo\offload() jobs, one at a time each

	spec    *spec.Spec
	rawSpec []byte

	nextID atomic.Int64
	calls  sync.Map // call id -> *call   (a request or an offload job awaiting PHP's answer)
	fibers sync.Map // fiber id -> *fiber (the goroutine that serves a PHP fiber)

	bootDone   chan error
	shutdownCh chan struct{}
	stopped    chan struct{}
	threadsWG  sync.WaitGroup
	threadErrs chan error
}

type pool int

const (
	poolWorker pool = iota
	poolOffload
)

type thread struct {
	id       int
	pool     pool
	inbox    chan []byte
	inflight atomic.Int64
}

// call is one unit of work sent to a PHP thread, waiting for cairo_respond().
type call struct {
	thread *thread
	resp   chan []byte
}

// fiber is the goroutine side of one PHP fiber.
type fiber struct {
	id     int64
	ctx    context.Context
	cancel context.CancelFunc
	thread *thread
	ops    chan op
	closed chan struct{} // child fibers only: closed by cairo_fiber_close
}

// op is an operation a fiber asked Go to perform while it is suspended.
type op struct {
	ID      int64       `json:"-"`
	Op      string      `json:"op"`
	Seconds float64     `json:"seconds"`
	Payload string      `json:"payload"`
	Fd      int         `json:"fd"`
	Write   bool        `json:"write"`
	Timeout float64     `json:"timeout"` // seconds; < 0 means none
	Network string      `json:"network"`
	Address string      `json:"address"`
	Host    string      `json:"host"`
	Read    []int       `json:"read"`
	WriteFD []int       `json:"write_fds"`
	TLS     *tlsOptions `json:"tls"`
}

var (
	engineMu sync.Mutex
	engine   *Engine
)

// Options configures Start.
type Options struct {
	ServerFile string
	// Threads and OffloadThreads override server.php when > 0.
	Threads        int
	OffloadThreads int
	// PHPIni is a php.ini file or directory; empty searches PHP's usual places.
	PHPIni string
	Logger *slog.Logger
}

// Start boots PHP, runs server.php once to collect its declaration (the
// declare phase), then starts the request threads and the offload threads.
func Start(opts Options) (*Engine, error) {
	engineMu.Lock()
	defer engineMu.Unlock()
	if engine != nil {
		return nil, errors.New("cairo: the PHP engine can only be started once per process")
	}
	serverFile, err := filepath.Abs(opts.ServerFile)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(serverFile); err != nil {
		return nil, fmt.Errorf("server file: %w", err)
	}
	runtimeFile, err := writeRuntime()
	if err != nil {
		return nil, err
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	e := &Engine{
		serverFile:  serverFile,
		runtimeFile: runtimeFile,
		phpIni:      opts.PHPIni,
		log:         logger,
		bootDone:    make(chan error, 1),
		shutdownCh:  make(chan struct{}),
		stopped:     make(chan struct{}),
		threadErrs:  make(chan error, 1),
	}
	engine = e

	// The boot thread initialises PHP, runs the declare phase and, at the end,
	// shuts PHP down. PHP requires both on the same OS thread.
	go e.bootThread()
	if err := <-e.bootDone; err != nil {
		return nil, err
	}

	nw := pick(opts.Threads, e.spec.Threads)
	no := pick(opts.OffloadThreads, e.spec.OffloadThreads)
	for i := 0; i < nw+no; i++ {
		t := &thread{id: i, pool: poolWorker, inbox: make(chan []byte, inboxSize)}
		if i >= nw {
			t.pool = poolOffload
			e.offload = append(e.offload, t)
		} else {
			e.workers = append(e.workers, t)
		}
		e.threads = append(e.threads, t)
	}
	for _, t := range e.threads {
		e.threadsWG.Add(1)
		if err := e.startThread(t); err != nil {
			e.threadsWG.Done()
			return nil, err
		}
	}
	e.log.Info("php threads started", "request_threads", nw, "offload_threads", no)
	return e, nil
}

func pick(override, declared int) int {
	if override > 0 {
		return override
	}
	if declared > 0 {
		return declared
	}
	return runtime.NumCPU()
}

// Spec is what server.php declared.
func (e *Engine) Spec() *spec.Spec { return e.spec }

// Errors reports a PHP thread that stopped unexpectedly.
func (e *Engine) Errors() <-chan error { return e.threadErrs }

func (e *Engine) bootThread() {
	runtime.LockOSThread()
	cfile := C.CString(e.runtimeFile)
	defer C.free(unsafe.Pointer(cfile))

	var cini *C.char
	if e.phpIni != "" {
		cini = C.CString(e.phpIni)
		defer C.free(unsafe.Pointer(cini))
	}
	status := C.cairo_boot(cfile, cini)
	switch {
	case status != 0:
		e.bootDone <- fmt.Errorf("server.php failed during the declare phase (exit status %d)", int(status))
	case e.spec == nil:
		e.bootDone <- errors.New("server.php did not return a Cairo\\App")
	default:
		e.bootDone <- nil
	}

	<-e.shutdownCh
	e.threadsWG.Wait()
	C.cairo_shutdown()
	close(e.stopped)
}

func (e *Engine) startThread(t *thread) error {
	cfile := C.CString(e.runtimeFile)
	defer C.free(unsafe.Pointer(cfile))
	if C.cairo_thread_start(C.int(t.id), cfile) != 0 {
		return fmt.Errorf("could not start php thread %d", t.id)
	}
	return nil
}

// Shutdown stops the PHP threads after the work they hold, then shuts PHP
// down. Callers should stop accepting requests first.
func (e *Engine) Shutdown(ctx context.Context) error {
	select {
	case <-e.shutdownCh:
		return nil
	default:
	}
	close(e.shutdownCh)
	stop, _ := json.Marshal(map[string]string{"type": "shutdown"})
	for _, t := range e.threads {
		t.inbox <- stop
	}
	select {
	case <-e.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Dispatch runs one request on a PHP request thread and returns PHP's answer.
//
// The calling goroutine becomes the request's root fiber: until PHP answers,
// it performs that fiber's I/O under ctx. ev must carry a "type" ("http" or
// "grpc"); Dispatch adds "id" and "deadline". If ctx ends first, Dispatch
// returns ctx.Err() and a helper goroutine keeps answering the fiber (with
// the context's error) until PHP finishes.
func (e *Engine) Dispatch(ctx context.Context, ev map[string]any) ([]byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	t := leastBusy(e.workers)
	c := &call{thread: t, resp: make(chan []byte, 1)}
	f := &fiber{ctx: ctx, cancel: cancel, thread: t, ops: make(chan op, 1)}
	id := e.nextID.Add(1)
	f.id = id
	e.calls.Store(id, c)
	e.fibers.Store(id, f)
	t.inflight.Add(1)

	ev["id"] = id
	if dl, ok := ctx.Deadline(); ok {
		ev["deadline"] = float64(dl.UnixNano()) / 1e9
	}
	b, err := json.Marshal(ev)
	if err != nil {
		e.forget(id, c, f)
		return nil, err
	}
	select {
	case t.inbox <- b:
	case <-ctx.Done():
		e.forget(id, c, f)
		return nil, ctx.Err()
	}
	for {
		select {
		case r := <-c.resp:
			e.fibers.Delete(id)
			cancel()
			return r, nil
		case o := <-f.ops:
			e.runOp(f, o)
		case <-ctx.Done():
			// Past the deadline, interrupt the thread until PHP answers: PHP
			// code that never waits on Go is stopped where it runs.
			deadline := errors.Is(ctx.Err(), context.DeadlineExceeded)
			go func() {
				var tick <-chan time.Time
				if deadline {
					C.cairo_interrupt(C.int(t.id))
					ticker := time.NewTicker(interruptEvery)
					defer ticker.Stop()
					tick = ticker.C
				}
				for {
					select {
					case <-c.resp:
						e.fibers.Delete(id)
						return
					case o := <-f.ops:
						e.runOp(f, o)
					case <-tick:
						C.cairo_interrupt(C.int(t.id))
					}
				}
			}()
			return nil, ctx.Err()
		}
	}
}

// interruptEvery is how often a request past its deadline interrupts its
// PHP thread until PHP answers.
const interruptEvery = 50 * time.Millisecond

func (e *Engine) forget(id int64, c *call, f *fiber) {
	e.calls.Delete(id)
	e.fibers.Delete(id)
	c.thread.inflight.Add(-1)
	f.cancel()
}

// runOp performs one fiber operation on the fiber's goroutine, then wakes the
// fiber on its PHP thread with the result.
func (e *Engine) runOp(f *fiber, o op) {
	ev := map[string]any{"type": "io", "io": o.ID}
	switch o.Op {
	case "sleep":
		timer := time.NewTimer(time.Duration(o.Seconds * float64(time.Second)))
		select {
		case <-timer.C:
		case <-f.ctx.Done():
			timer.Stop()
			ev["error"] = ctxError(f.ctx)
		}
	case "offload":
		res, err := e.runOffload(f.ctx, o.Payload)
		if err != nil {
			ev["error"] = ctxError(f.ctx)
		} else {
			ev["result"] = res
		}
	case "wait":
		// A hooked socket read or write: wait until the descriptor is ready.
		ev["result"] = "ready"
		p, err := getPoller()
		if err == nil {
			timeout := time.Duration(-1)
			if o.Timeout >= 0 {
				timeout = time.Duration(o.Timeout * float64(time.Second))
			}
			err = p.wait(f.ctx, o.Fd, o.Write, timeout)
		}
		switch {
		case err == nil:
		case errors.Is(err, errPollTimeout):
			ev["result"] = "timeout"
		case f.ctx.Err() != nil:
			ev["error"] = ctxError(f.ctx)
		default:
			ev["error"] = err.Error()
		}
	case "waitset":
		// A hooked curl transfer: wait for any of curl's sockets.
		ev["result"] = "ready"
		p, err := getPoller()
		if err == nil {
			timeout := time.Duration(-1)
			if o.Timeout >= 0 {
				timeout = time.Duration(o.Timeout * float64(time.Second))
			}
			err = p.waitSet(f.ctx, o.Read, o.WriteFD, timeout)
		}
		switch {
		case err == nil:
		case errors.Is(err, errPollTimeout):
			ev["result"] = "timeout"
		case f.ctx.Err() != nil:
			ev["error"] = ctxError(f.ctx)
		default:
			ev["error"] = err.Error()
		}
	case "dial":
		// A hooked connect: DNS and connect run here, on the fiber's goroutine.
		timeout := time.Duration(0)
		if o.Timeout > 0 {
			timeout = time.Duration(o.Timeout * float64(time.Second))
		}
		var fd int
		var err error
		if o.Network == "tls" && o.TLS != nil {
			fd, err = dialTLS(f.ctx, o.Address, timeout, *o.TLS)
		} else {
			fd, err = dialFD(f.ctx, o.Network, o.Address, timeout)
		}
		switch {
		case err == nil:
			ev["result"] = map[string]any{"fd": fd}
		case f.ctx.Err() != nil:
			ev["error"] = ctxError(f.ctx)
		default:
			ev["result"] = map[string]any{"err": err.Error()}
		}
	case "starttls":
		// stream_socket_enable_crypto(): the handshake runs here.
		timeout := time.Duration(0)
		if o.Timeout > 0 {
			timeout = time.Duration(o.Timeout * float64(time.Second))
		}
		var opts tlsOptions
		if o.TLS != nil {
			opts = *o.TLS
		}
		fd, err := startTLS(f.ctx, o.Fd, o.Host, timeout, opts)
		switch {
		case err == nil:
			ev["result"] = map[string]any{"fd": fd}
		case f.ctx.Err() != nil:
			ev["error"] = ctxError(f.ctx)
		default:
			ev["result"] = map[string]any{"err": err.Error()}
		}
	case "pg":
		// A PDO pgsql call: runs here, on the fiber's goroutine, so the
		// request's deadline cancels the query.
		req, err := pgRequestFrom([]byte(o.Payload))
		if err != nil {
			ev["error"] = err.Error()
			break
		}
		resp := pgDo(f.ctx, req)
		if f.ctx.Err() != nil {
			if id, ok := resp["result"].(int64); ok {
				if r := goPgTake(C.longlong(id)); r != nil {
					C.cairo_pg_result_free(r)
				}
			}
			ev["error"] = ctxError(f.ctx)
			break
		}
		ev["result"] = resp
	case "lookup":
		ips, err := lookupIPv4(f.ctx, o.Host)
		switch {
		case err == nil:
			ev["result"] = ips
		case f.ctx.Err() != nil:
			ev["error"] = ctxError(f.ctx)
		default:
			ev["result"] = []string{}
		}
	default:
		h, ok := goOps[o.Op]
		if !ok {
			ev["error"] = "unknown operation " + o.Op
			break
		}
		res, err := h(f.ctx, []byte(o.Payload))
		switch {
		case f.ctx.Err() != nil:
			ev["error"] = ctxError(f.ctx)
		case err != nil:
			ev["error"] = err.Error()
		default:
			ev["result"] = res
		}
	}
	e.post(f.thread, ev)
}

// runOffload sends a job to the offload pool and waits for its result.
func (e *Engine) runOffload(ctx context.Context, payload string) (string, error) {
	if len(e.offload) == 0 {
		return "", errors.New("no offload threads")
	}
	t := leastBusy(e.offload)
	c := &call{thread: t, resp: make(chan []byte, 1)}
	id := e.nextID.Add(1)
	e.calls.Store(id, c)
	t.inflight.Add(1)
	b, _ := json.Marshal(map[string]any{"type": "offload", "id": id, "payload": payload})
	select {
	case t.inbox <- b:
	case <-ctx.Done():
		e.calls.Delete(id)
		t.inflight.Add(-1)
		return "", ctx.Err()
	}
	select {
	case r := <-c.resp:
		var out struct {
			Payload string `json:"payload"`
		}
		if err := json.Unmarshal(r, &out); err != nil {
			return "", err
		}
		return out.Payload, nil
	case <-ctx.Done():
		// The job keeps running on its thread; its answer is dropped.
		return "", ctx.Err()
	}
}

// leastBusy returns the thread with the fewest calls in flight.
func leastBusy(ts []*thread) *thread {
	best := ts[0]
	min := best.inflight.Load()
	for _, t := range ts[1:] {
		if n := t.inflight.Load(); n < min {
			best, min = t, n
		}
	}
	return best
}

func (e *Engine) post(t *thread, ev any) {
	b, err := json.Marshal(ev)
	if err != nil {
		e.log.Error("encode event", "err", err)
		return
	}
	t.inbox <- b
}

func ctxError(ctx context.Context) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "deadline_exceeded"
	}
	return "canceled"
}

func writeRuntime() (string, error) {
	dir, err := os.MkdirTemp("", "cairo-runtime-")
	if err != nil {
		return "", err
	}
	f := filepath.Join(dir, "runtime.php")
	if err := os.WriteFile(f, php.Runtime, 0o600); err != nil {
		return "", err
	}
	return f, nil
}

// ---- functions PHP calls (see cairo.c) ----

//export goThreadDone
func goThreadDone(tid C.int, status C.int) {
	e := engine
	defer e.threadsWG.Done()
	select {
	case <-e.shutdownCh:
		return
	default:
	}
	err := fmt.Errorf("php thread %d stopped unexpectedly (exit status %d)", int(tid), int(status))
	select {
	case e.threadErrs <- err:
	default:
	}
}

// goPHPLog receives PHP's log lines (errors, warnings, error_log()) from the
// cairo SAPI; syslogType is the syslog priority PHP gives them.
//
//export goPHPLog
func goPHPLog(tid C.int, msg *C.char, syslogType C.int) {
	e := engine
	if e == nil {
		return
	}
	level := slog.LevelInfo
	switch {
	case syslogType <= 3: // LOG_ERR and above
		level = slog.LevelError
	case syslogType == 4: // LOG_WARNING
		level = slog.LevelWarn
	case syslogType == 7: // LOG_DEBUG
		level = slog.LevelDebug
	}
	e.log.Log(context.Background(), level, C.GoString(msg), "source", "php", "thread", int(tid))
}

//export goCairoInfo
func goCairoInfo(tid C.int) *C.char {
	e := engine
	mode := "declare"
	if tid >= 0 {
		mode = "serve"
		if e.threads[int(tid)].pool == poolOffload {
			mode = "offload"
		}
	}
	b, _ := json.Marshal(map[string]any{"mode": mode, "server": e.serverFile, "thread": int(tid)})
	return C.CString(string(b))
}

//export goCairoSpec
func goCairoSpec(tid C.int, data *C.char, n C.int) *C.char {
	e := engine
	raw := C.GoBytes(unsafe.Pointer(data), n)
	if tid < 0 {
		s, err := spec.Parse(raw)
		if err != nil {
			return C.CString(err.Error())
		}
		e.spec, e.rawSpec = s, raw
		return nil
	}
	if !bytes.Equal(raw, e.rawSpec) {
		return C.CString(fmt.Sprintf("php thread %d built a different server declaration than the boot thread; server.php must declare the same servers and routes every time it runs", int(tid)))
	}
	return nil
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

//export goNextEvent
func goNextEvent(tid C.int) *C.char {
	b := <-engine.threads[int(tid)].inbox
	return C.CString(string(b))
}

//export goRespond
func goRespond(id C.longlong, data *C.char, n C.int) {
	e := engine
	v, ok := e.calls.LoadAndDelete(int64(id))
	if !ok {
		return
	}
	c := v.(*call)
	c.thread.inflight.Add(-1)
	c.resp <- C.GoBytes(unsafe.Pointer(data), n)
}

// goFiberOpen gives a new PHP fiber (from Cairo\async) its own goroutine,
// with a child of the parent fiber's context.
//
//export goFiberOpen
func goFiberOpen(tid C.int, parent C.longlong) C.longlong {
	e := engine
	pctx := context.Background()
	if v, ok := e.fibers.Load(int64(parent)); ok {
		pctx = v.(*fiber).ctx
	}
	ctx, cancel := context.WithCancel(pctx)
	f := &fiber{
		id: e.nextID.Add(1), ctx: ctx, cancel: cancel, thread: e.threads[int(tid)],
		ops: make(chan op, 1), closed: make(chan struct{}),
	}
	e.fibers.Store(f.id, f)
	// The goroutine lives as long as the fiber, even after its context ends:
	// operations then fail fast with the context's error, so the fiber always wakes.
	go func() {
		for {
			select {
			case o := <-f.ops:
				e.runOp(f, o)
			case <-f.closed:
				return
			}
		}
	}()
	return C.longlong(f.id)
}

// goFiberClose ends a fiber's goroutine and cancels its context.
//
//export goFiberClose
func goFiberClose(id C.longlong) {
	if v, ok := engine.fibers.LoadAndDelete(int64(id)); ok {
		f := v.(*fiber)
		f.cancel()
		if f.closed != nil {
			close(f.closed)
		}
	}
}

// goFiberOp hands an operation to the fiber's goroutine and returns its id.
// The PHP fiber suspends right after; the goroutine wakes it when done.
//
//export goFiberOp
func goFiberOp(id C.longlong, data *C.char, n C.int) C.longlong {
	e := engine
	opID := e.nextID.Add(1)
	v, ok := e.fibers.Load(int64(id))
	if !ok {
		return -1
	}
	var o op
	if err := json.Unmarshal(C.GoBytes(unsafe.Pointer(data), n), &o); err != nil {
		return -1
	}
	o.ID = opID
	v.(*fiber).ops <- o
	return C.longlong(opID)
}

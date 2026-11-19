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
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/cairophp/cairo/internal/spec"
	"github.com/cairophp/cairo/internal/wire"
	"github.com/cairophp/cairo/php"
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

	table   [maxThreads]atomic.Pointer[thread] // live PHP threads by thread id
	workers []atomic.Pointer[thread]           // run requests as fibers; a slot's thread is replaced on recycle
	offload []atomic.Pointer[thread]           // run Cairo\offload() jobs, one at a time each

	tidMu    sync.Mutex
	freeTids []int // thread ids of recycled threads, for reuse
	nextTid  int

	spec    *spec.Spec
	rawSpec []byte

	// slots caps requests in flight (App::maxInFlight() per request thread);
	// nil when there is no cap.
	slots chan struct{}
	// idle holds the request threads free to take a request, in isolated
	// mode; nil in fiber mode.
	idle idleSlots

	// slowAfter is App::slowRequests() and heavyAfter App::heavyRoutes()
	// (0: off). Either one times each request (reqTiming).
	slowAfter  time.Duration
	heavyAfter time.Duration

	nextID atomic.Int64
	calls  sync.Map // call id -> *call   (a request or an offload job awaiting PHP's answer)
	fibers sync.Map // fiber id -> *fiber (the goroutine that serves a PHP fiber)

	bootDone   chan error
	lifeMu     sync.Mutex // orders thread starts against Shutdown
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
	slot     int // index in its pool
	inbox    chan event
	inflight atomic.Int64

	// The blocking detector (detect.go): when the thread last took an event
	// (unix ns; 0 while it waits for one), and whether that stretch was reported.
	busySince atomic.Int64
	reported  atomic.Bool
	// The request whose fiber this thread resumed last (slowlog.go); only
	// the thread itself uses it, in goNextEvent.
	running *reqTiming

	// Supervision (see supervise.go).
	sendMu   sync.RWMutex // held for reading while a request is queued; retire takes it to set retired
	retired  bool         // no new requests may be queued here
	retiring atomic.Bool  // a replacement was asked for; the thread exits once idle
	served   atomic.Int64 // requests and jobs the thread has taken
	crashes  int          // crashes in a row without taking work in between
	bootMark int64        // served when the thread last started
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
	if nw+no > maxThreads/2 {
		return nil, fmt.Errorf("too many PHP threads: %d (at most %d)", nw+no, maxThreads/2)
	}
	if os.Getenv("GOMAXPROCS") == "" {
		// A PHP thread calling into Go (to answer a request, to start I/O)
		// needs a free P. With only one P per CPU, net/http's goroutines hold
		// them all under load and PHP threads queue for one.
		runtime.GOMAXPROCS(runtime.GOMAXPROCS(0) + nw + no)
	}
	if os.Getenv("GOGC") == "" {
		// Go's live heap is small (request data lives in PHP), so with the
		// default of 100 it collects every few hundred requests. 400 measured
		// about 10% more requests per second for about 12 MiB more memory.
		debug.SetGCPercent(400)
	}
	if e.spec.MaxInFlight > 0 {
		e.slots = make(chan struct{}, e.spec.MaxInFlight*nw)
	}
	if e.spec.Isolated {
		e.idle = newIdleSlots(nw)
	}
	e.workers = make([]atomic.Pointer[thread], nw)
	e.offload = make([]atomic.Pointer[thread], no)
	var all []*thread
	for i := 0; i < nw+no; i++ {
		tid, _ := e.allocTid()
		t := &thread{id: tid, pool: poolWorker, slot: i, inbox: make(chan event, inboxSize)}
		if i >= nw {
			t.pool, t.slot = poolOffload, i-nw
			e.offload[t.slot].Store(t)
		} else {
			e.workers[t.slot].Store(t)
		}
		e.table[tid].Store(t)
		all = append(all, t)
	}
	for _, t := range all {
		e.threadsWG.Add(1)
		if err := e.startThread(t); err != nil {
			e.threadsWG.Done()
			return nil, err
		}
	}
	e.log.Info("php threads started", "request_threads", nw, "offload_threads", no)
	e.slowAfter = time.Duration(e.spec.SlowRequests * float64(time.Second))
	e.heavyAfter = time.Duration(e.spec.HeavyRoutes * float64(time.Second))
	if e.spec.DetectBlocking > 0 {
		go e.detectBlocking(time.Duration(e.spec.DetectBlocking * float64(time.Second)))
	}
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

// Errors reports a PHP thread that could not be restarted (see supervise.go).
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
	e.lifeMu.Lock()
	select {
	case <-e.shutdownCh:
		e.lifeMu.Unlock()
		return nil
	default:
	}
	close(e.shutdownCh)
	e.lifeMu.Unlock()
	stop := wire.Encode(map[string]string{"type": "shutdown"})
	for i := range e.table {
		if t := e.table[i].Load(); t != nil {
			t.inbox <- event{data: stop}
		}
	}
	select {
	case <-e.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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
	// Next to it: loaded by runtime.php's autoloader on first use.
	for name, code := range map[string][]byte{"revolt.php": php.Revolt, "cache.php": php.Cache, "pool.php": php.Pool, "queries.php": php.Queries, "leaks.php": php.Leaks, "request_local.php": php.RequestLocal} {
		if err := os.WriteFile(filepath.Join(dir, name), code, 0o600); err != nil {
			return "", err
		}
	}
	return f, nil
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
		if e.table[int(tid)].Load().pool == poolOffload {
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

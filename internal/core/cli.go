package core

/*
#include <stdlib.h>
#include "cli.h"
*/
import "C"

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"unsafe"
)

// CLIMode is what a run of PHP's command line does.
type CLIMode int

const (
	CLIFile  CLIMode = C.CAIRO_CLI_FILE  // runs the script Args[0]
	CLIStdin CLIMode = C.CAIRO_CLI_STDIN // runs the script it reads from standard input
	CLICode  CLIMode = C.CAIRO_CLI_CODE  // runs Code
	CLILint  CLIMode = C.CAIRO_CLI_LINT  // checks the syntax of the files in Args
)

// What PHP's own command line sets whatever php.ini says (HARDCODED_INI in
// sapi/cli/php_cli.c).
const cliINI = "html_errors=0\nimplicit_flush=1\noutput_buffering=0\nmax_execution_time=0\nmax_input_time=-1\nregister_argc_argv=1\n"

// CLI is one run of PHP's command line.
type CLI struct {
	Mode CLIMode
	Code string
	// Args is $argv: the script as it was named, then its arguments. For
	// CLILint it is the files to check; none checks standard input.
	Args []string
	// INI is php.ini lines that no php.ini overrides (-d).
	INI string
	// INIPath is a php.ini file or directory; empty searches PHP's usual places.
	INIPath string
	// NoINI reads no php.ini at all.
	NoINI bool
	// NoScheduler runs the script as PHP does, outside Cairo's scheduler:
	// for a program that brings its own event loop for fibers, which would
	// suspend the script's fiber where nothing of Cairo's resumes it.
	NoScheduler bool
	Logger      *slog.Logger
}

// RunCLI runs PHP as its own command line does and returns PHP's exit
// status. No server starts: one thread runs the script, as the one request
// it ever takes. The script is a fiber of that thread's scheduler, and what
// its fibers ask Go for runs as operations, under a context that ends with
// the script; with NoScheduler there is none of that, and what Cairo does in
// Go the thread waits for. Like Start, it can run once per process.
func RunCLI(o CLI) (int, error) {
	engineMu.Lock()
	defer engineMu.Unlock()
	if engine != nil {
		return 0, errors.New("cairo: the PHP engine can only be started once per process")
	}
	binary, err := os.Executable()
	if err != nil {
		return 0, err
	}
	runtimeFile, err := writeRuntime()
	if err != nil {
		return 0, err
	}
	// For a run that did not come to load them (goCLILoaded).
	defer os.RemoveAll(filepath.Dir(runtimeFile))
	logger := o.Logger
	if logger == nil {
		logger = slog.Default()
	}
	engine = &Engine{runtimeFile: runtimeFile, log: logger, cli: true}
	if o.Mode != CLILint && !o.NoScheduler {
		engine.hostScript()
	}

	var free []unsafe.Pointer
	defer func() {
		for _, p := range free {
			C.free(p)
		}
	}()
	cstring := func(s string) *C.char {
		p := C.CString(s)
		free = append(free, unsafe.Pointer(p))
		return p
	}
	// One more than the arguments: C's argv ends in NULL.
	argv := (**C.char)(C.calloc(C.size_t(len(o.Args)+1), C.size_t(unsafe.Sizeof((*C.char)(nil)))))
	free = append(free, unsafe.Pointer(argv))
	for i, arg := range o.Args {
		unsafe.Slice(argv, len(o.Args))[i] = cstring(arg)
	}
	run := C.cairo_cli_options{
		mode:    C.int(o.Mode),
		runtime: cstring(runtimeFile),
		code:    cstring(o.Code),
		argc:    C.int(len(o.Args)),
		argv:    argv,
		ini:     cstring(cliINI + o.INI),
		binary:  cstring(binary),
	}
	if o.INIPath != "" {
		run.ini_path = cstring(o.INIPath)
	}
	if o.NoINI {
		run.ini_ignore = 1
	}
	if engine.script != 0 {
		run.scheduled = 1
	}
	return int(C.cairo_cli(&run)), nil
}

// hostScript makes the Go side of the thread that runs the script: PHP thread
// 0, which holds one call, the script, for as long as it runs (submit looks
// up there what an operation runs under). Nothing is queued for the thread
// but the results of its operations, and it answers nothing: no goroutine
// collects.
func (e *Engine) hostScript() {
	t := e.newThread(0, poolWorker, 0)
	ctx, cancel := context.WithCancel(context.Background())
	e.script, e.scriptEnd = e.nextID.Add(1), cancel
	t.calls[e.script] = &call{id: e.script, thread: t, ctx: ctx}
	setLook()
}

// goCLIEnded is the script's thread saying that the script has ended: what
// its fibers still wait for in Go ends, as it does behind a request that was
// answered.
//
//export goCLIEnded
func goCLIEnded() {
	if engine.scriptEnd != nil {
		engine.scriptEnd()
	}
}

// goCLILoaded is called once the script's request has loaded Cairo's runtime,
// all of it, and the script begins. Its files go now and not when the command ends: nothing is left
// behind by a command that a signal ends, and many commands are run.
//
//export goCLILoaded
func goCLILoaded() {
	os.RemoveAll(filepath.Dir(engine.runtimeFile))
}

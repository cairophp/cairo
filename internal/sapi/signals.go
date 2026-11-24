package sapi

/*
#include "signals.h"
*/
import "C"

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/cairophp/cairo/internal/core"
)

// Signals for the scripts of `cairo php`. PHP's pcntl says which signals a
// script handles (signals.c tells Go around pcntl_signal()); os/signal
// receives them, on whatever thread they land, and deliverSignals brings
// each to the script's thread, where PHP runs the script's handler.

var (
	signalsOnce sync.Once
	signalsIn   chan os.Signal           // what os/signal has for the script
	signalsWoke = make(chan struct{}, 1) // a signal waits for a fiber to raise it
)

func init() {
	registerC(&C.cairo_signals)
	registerPHP("signals.php", `Cairo\Internal\Signals`)
	core.RegisterOp("signals.wait", waitForSignal)
}

// goSignalListen is called before PHP installs the script's handler of
// signo.
//
//export goSignalListen
func goSignalListen(signo C.int) {
	signalsOnce.Do(func() {
		// Signals of one kind that nobody took yet count once, as they do for the system.
		signalsIn = make(chan os.Signal, 64)
		// A command is one process: this ends with it.
		go deliverSignals()
	})
	signal.Notify(signalsIn, syscall.Signal(signo))
}

// goSignalStop is called once the script no longer handles signo: Go's own
// way with the signal is back.
//
//export goSignalStop
func goSignalStop(signo C.int) {
	signal.Reset(syscall.Signal(signo))
}

func deliverSignals() {
	// While a signal waits for a fiber, the thread is asked again: the
	// interrupt may have found it inside Cairo's own code, where no handler
	// runs, and asking again from there would spin.
	var again <-chan time.Time
	for {
		select {
		case sig := <-signalsIn:
			signo := sig.(syscall.Signal)
			if C.cairo_signal_deliver(C.int(signo)) == 0 {
				// PHP dropped the handler without a word, as it does when the
				// script has ended: the signal does what it does without one.
				signal.Reset(signo)
				_ = syscall.Kill(syscall.Getpid(), signo)
				continue
			}
			select {
			case signalsWoke <- struct{}{}:
			default:
			}
			again = time.After(time.Millisecond)
		case <-again:
			again = nil
			if C.cairo_signals_interrupt_again() != 0 {
				again = time.After(time.Millisecond)
			}
		}
	}
}

// waitForSignal is what the fiber of Cairo\Internal\Signals waits in, until
// a signal is there for it to raise or the script ends.
func waitForSignal(ctx context.Context, _ []byte) (any, error) {
	select {
	case <-signalsWoke:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

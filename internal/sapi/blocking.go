package sapi

/*
#include "blocking.h"
*/
import "C"

import (
	"errors"
	"time"

	"github.com/cairophp/cairo/internal/core"
	"github.com/cairophp/cairo/internal/spec"
)

// The development aid behind App::detectBlocking(): a request thread that
// runs PHP for longer than the threshold without coming back for its next
// event is blocked, by a CPU loop or a blocking call Cairo does not hook, and
// every other fiber on it waits. The thread is asked to report: at its next
// safe point the running fiber logs the time and its stack
// (Cairo\Internal\reportBlocking, in php/blocking.php).

func init() {
	registerC(&C.cairo_blocking)
	registerPHP("blocking.php")
	core.RegisterStart(startBlockingDetector)
}

func startBlockingDetector(e *core.Engine) error {
	threshold, err := blockingThreshold(e.Spec())
	if err != nil {
		return err
	}
	if threshold > 0 {
		go detectBlocking(e, threshold)
	}
	return nil
}

// blockingThreshold is the threshold server.php declared (0: off).
func blockingThreshold(s *spec.Spec) (time.Duration, error) {
	var declared struct {
		After float64 `json:"after"` // seconds
	}
	if err := s.Section("blocking", &declared); err != nil {
		return 0, err
	}
	if declared.After < 0 {
		return 0, errors.New("detectBlocking must be 0 or more")
	}
	return time.Duration(declared.After * float64(time.Second)), nil
}

// busyThreads is what the detector needs of the engine.
type busyThreads interface {
	Stopping() <-chan struct{}
	EachBusyThread(fn func(tid int, since time.Time))
	InterruptThread(tid int)
}

// detectBlocking asks each thread once per busy stretch, until the engine
// stops.
func detectBlocking(e busyThreads, threshold time.Duration) {
	tick := time.NewTicker(max(threshold/4, 10*time.Millisecond))
	defer tick.Stop()
	asked := map[int]time.Time{} // thread id => the start of the stretch it was last asked about
	for {
		select {
		case <-e.Stopping():
			return
		case now := <-tick.C:
			e.EachBusyThread(func(tid int, since time.Time) {
				if busy := now.Sub(since); busy >= threshold && !asked[tid].Equal(since) {
					asked[tid] = since
					C.cairo_report_blocking(C.int(tid), C.int(busy.Milliseconds()))
					e.InterruptThread(tid)
				}
			})
		}
	}
}

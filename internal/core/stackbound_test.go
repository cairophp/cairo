package core

import (
	"os"
	"runtime"
	"syscall"
	"testing"
)

// offFirstThread runs f on an OS thread that is not the process's first,
// whose stack grows and is never remembered.
func offFirstThread(f func()) {
	done := make(chan struct{})
	run := func() {
		defer close(done)
		f()
	}
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if syscall.Gettid() != os.Getpid() {
			run()
			return
		}
		// This goroutine keeps the first thread, so the next one gets another.
		go func() {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			run()
		}()
		<-done
	}()
	<-done
}

// A call from a fiber into Go makes Go's runtime ask for the thread's stack
// again. Only the first question of a thread may reach glibc, and the later
// answers must be the same stack.
func TestStackBoundReadOnce(t *testing.T) {
	offFirstThread(func() {
		lo, hi, first := stackBound()
		if first < 0 || first > 1 || lo == 0 || lo >= hi {
			t.Fatalf("stack %#x-%#x after %d lookups", lo, hi, first)
		}
		for i := 0; i < 3; i++ {
			lo2, hi2, lookups := stackBound()
			if lo2 != lo || hi2 != hi {
				t.Errorf("stack %#x-%#x, first answer %#x-%#x", lo2, hi2, lo, hi)
			}
			if lookups != first {
				t.Errorf("%d lookups, want %d: the stack was read again", lookups, first)
			}
		}
	})
}

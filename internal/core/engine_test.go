package core

import "testing"

// What an extension's goroutine ends on is the channel that Shutdown closes.
func TestStopping(t *testing.T) {
	e := &Engine{shutdownCh: make(chan struct{})}
	select {
	case <-e.Stopping():
		t.Fatal("Stopping() is closed before Shutdown began")
	default:
	}
	close(e.shutdownCh)
	select {
	case <-e.Stopping():
	default:
		t.Fatal("Stopping() is open after Shutdown closed its channel")
	}
}

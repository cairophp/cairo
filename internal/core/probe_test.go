package core

import "testing"

// One probe per request is all the engine carries.
func TestProbeRequestsOnce(t *testing.T) {
	e := &Engine{}
	e.ProbeRequests(func() Probe { return nil })
	defer func() {
		if recover() == nil {
			t.Error("a second ProbeRequests did not panic")
		}
	}()
	e.ProbeRequests(func() Probe { return nil })
}

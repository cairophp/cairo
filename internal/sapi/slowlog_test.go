package sapi

import (
	"testing"
	"time"
)

func TestOpKind(t *testing.T) {
	for op, want := range map[string]waitKind{
		"pg": waitSQL, "sql.query": waitSQL, "sql.begin": waitSQL,
		"http": waitHTTP, "waitset": waitHTTP, "wait": waitSocket,
		"dial": waitConnect, "lookup": waitConnect, "starttls": waitConnect,
		"sleep": waitSleep, "park": waitChannel, "offload": waitOffload,
		"grpc.call": waitGRPC, "cache.purge": waitOther,
	} {
		if got := opKind(op); got != want {
			t.Errorf("opKind(%q) = %s, want %s", op, waitKindNames[got], waitKindNames[want])
		}
	}
}

// Two waits that overlap count once towards the time spent waiting.
func TestReqTimingOverlap(t *testing.T) {
	r := &reqTiming{start: time.Now().Add(-100 * time.Millisecond)}
	r.since = r.start
	a := r.begin()
	b := r.begin()
	r.end(waitSleep, a)
	r.end(waitSleep, b)
	if n := r.count[waitSleep].Load(); n != 2 {
		t.Fatalf("count = %d", n)
	}
	if php := r.phpTime(time.Now()); php < 95*time.Millisecond || php > 200*time.Millisecond {
		t.Errorf("phpTime = %s, want about 100ms (the waits took no time)", php)
	}
}

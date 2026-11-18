package cachetag

import "testing"

func TestPurge(t *testing.T) {
	a, b := Counter("a"), Counter("b")
	if Counter("a") != a {
		t.Fatal("a tag got two counters")
	}
	Purge("a", "never-declared")
	if a.Load() != 1 || b.Load() != 0 {
		t.Fatalf("a=%d b=%d, want 1 and 0", a.Load(), b.Load())
	}
	if _, ok := tags.Load("never-declared"); ok {
		t.Fatal("purging an unknown tag created it")
	}
}

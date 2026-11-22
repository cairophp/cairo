package sapi

import (
	"testing"
	"time"
)

func TestLogOnceDue(t *testing.T) {
	var l logOnce
	now := time.Unix(0, 0)
	for _, c := range []struct {
		key   string
		after time.Duration
		want  bool
	}{
		{"GET /a\x00q", 0, true},
		{"GET /a\x00q", 30 * time.Second, false}, // once a minute per key
		{"GET /b\x00q", 30 * time.Second, true},
		{"GET /a\x00q", time.Minute, true},
	} {
		if got := l.due(c.key, now.Add(c.after)); got != c.want {
			t.Errorf("due(%q) after %s = %v, want %v", c.key, c.after, got, c.want)
		}
	}
	for i := 0; i < logOnceMax+10; i++ {
		l.due(string(rune(i)), now)
	}
	if len(l.last) > logOnceMax {
		t.Errorf("%d keys remembered, want at most %d", len(l.last), logOnceMax)
	}
}

package httpserver

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// heard is a listener that keeps what it is handed, or refuses it.
type heard struct {
	subscription
	got     chan string
	refuse  bool
	dropped chan struct{}
}

func hear(t *testing.T, names ...string) *heard {
	t.Helper()
	h := &heard{got: make(chan string, 256), dropped: make(chan struct{}, 1)}
	if err := join(h, &h.subscription, names); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { leave(h, &h.subscription, nil) })
	return h
}

func (h *heard) deliver(m *publication) bool {
	if h.refuse {
		return false
	}
	h.got <- string(m.asEvent())
	return true
}

func (h *heard) drop() { h.dropped <- struct{}{} }

func (h *heard) next(t *testing.T) string {
	t.Helper()
	select {
	case event := <-h.got:
		return event
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
		return ""
	}
}

func TestTopicsHandOutInOrder(t *testing.T) {
	news, both, other := hear(t, "t.news"), hear(t, "t.news", "t.other"), hear(t, "t.other")
	for i := 0; i < 100; i++ {
		n, err := Publish(context.Background(), "t.news", []byte(fmt.Sprint(i)), "", "")
		if err != nil || n != 2 {
			t.Fatalf("publish %d: %d listen (%v), want 2", i, n, err)
		}
	}
	if n, queued, err := TryPublish("t.other", []byte("last"), "", ""); n != 2 || !queued || err != nil {
		t.Fatalf("TryPublish = %d, %v, %v", n, queued, err)
	}
	for i := 0; i < 100; i++ {
		want := fmt.Sprintf("data: %d\n\n", i)
		if got := news.next(t); got != want {
			t.Fatalf("event %d = %q", i, got)
		}
		if got := both.next(t); got != want {
			t.Fatalf("event %d of the listener of two topics = %q", i, got)
		}
	}
	if got := other.next(t); got != "data: last\n\n" {
		t.Errorf("the other topic's listener got %q", got)
	}
	if n, _, _ := TryPublish("t.nobody", nil, "", ""); n != 0 {
		t.Errorf("%d listen to a topic that nobody joined", n)
	}
}

func TestTopicEventForm(t *testing.T) {
	for _, tc := range []struct {
		data, event, id, want string
	}{
		{"one", "", "", "data: one\n\n"},
		{"", "", "", "data: \n\n"},
		{"two\nlines", "price", "7", "event: price\nid: 7\ndata: two\ndata: lines\n\n"},
		{"a\r\nb\rc", "", "", "data: a\ndata: b\ndata: c\n\n"},
	} {
		m := &publication{data: []byte(tc.data), event: tc.event, id: tc.id}
		if got := string(m.asEvent()); got != tc.want {
			t.Errorf("event of %q = %q, want %q", tc.data, got, tc.want)
		}
	}
}

func TestTopicsRefuseWhatIsNotValid(t *testing.T) {
	for name, publish := range map[string]func() error{
		"no topic":         func() error { _, _, err := TryPublish("", nil, "", ""); return err },
		"a long topic":     func() error { _, _, err := TryPublish(strings.Repeat("t", MaxTopicName+1), nil, "", ""); return err },
		"too much data":    func() error { _, _, err := TryPublish("t", make([]byte, MaxPublication+1), "", ""); return err },
		"an event's lines": func() error { _, _, err := TryPublish("t", nil, "a\nb", ""); return err },
		"a long event":     func() error { _, _, err := TryPublish("t", nil, strings.Repeat("e", MaxTopicName+1), ""); return err },
		"an id's lines":    func() error { _, err := Publish(context.Background(), "t", nil, "", "1\r2"); return err },
	} {
		if publish() == nil {
			t.Errorf("%s: published", name)
		}
	}
}

func TestListenerJoinsAllOrNothing(t *testing.T) {
	h := hear(t, "t.first")
	many := make([]string, maxTopics)
	for i := range many {
		many[i] = fmt.Sprint("t.many.", i)
	}
	if err := join(h, &h.subscription, many); err == nil {
		t.Fatalf("joined %d topics", maxTopics+1)
	}
	if len(h.names) != 1 || listeners("t.many.0") != 0 {
		t.Errorf("after a refused join: %v, %d listen to the first of them", h.names, listeners("t.many.0"))
	}
	if err := join(h, &h.subscription, append(many[:maxTopics-1], "t.first", many[0])); err != nil {
		t.Fatalf("a join that names topics twice: %v", err)
	}
	leave(h, &h.subscription, []string{"t.first", "t.never"})
	if len(h.names) != maxTopics-1 || listeners("t.first") != 0 {
		t.Errorf("after leaving one topic: %d names, %d listen to it", len(h.names), listeners("t.first"))
	}
	leave(h, &h.subscription, nil)
	if listeners("t.many.0") != 0 {
		t.Error("still listening after it left")
	}
	if err := join(h, &h.subscription, []string{"t.first"}); err == nil {
		t.Error("a listener that left for good joined again")
	}
}

func TestSlowListenerIsDropped(t *testing.T) {
	slow, fine := hear(t, "t.slow"), hear(t, "t.slow")
	slow.refuse = true
	if _, err := Publish(context.Background(), "t.slow", []byte("x"), "", ""); err != nil {
		t.Fatal(err)
	}
	if got := fine.next(t); got != "data: x\n\n" {
		t.Errorf("the listener that keeps up got %q", got)
	}
	select {
	case <-slow.dropped:
	case <-time.After(5 * time.Second):
		t.Fatal("the listener that refused was not dropped")
	}
}

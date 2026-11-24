package httpserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Topics: PHP publishes a message once (Publish) and Go hands it to every
// client that listens to its topic, with no PHP in between. A listener is a
// WebSocket that subscribed (SubscribeSocket), or a Server-Sent Events
// answer that Go serves by itself (topicsAnswer): PHP decides who may listen
// to what and its fiber ends, so such a client costs no PHP memory at all.
//
// Publishing never waits for a client. PHP queues a message with one direct
// call (TryPublish) and one goroutine hands the messages out, in the order
// they were published. Each listener has the queue of its connection, and
// one that has no room left there is dropped: it reads too slowly for what
// its topics carry, and connects again.
//
// The topics are this process's. Nothing is kept: a client hears what is
// published while it listens.

const (
	MaxTopicName    = 256     // bytes, of an event's name and id too
	maxTopics       = 64      // that one client listens to
	MaxPublication  = 1 << 20 // bytes of data
	publishQueue    = 1024    // messages that wait to be handed out
	topicsKeepAlive = ": \n\n"
)

type listener interface {
	// deliver queues m for the client without waiting; false when it cannot.
	deliver(m *publication) bool
	// drop ends a listener that deliver has failed for.
	drop()
}

// subscription is the topics of one listener, which embeds it.
type subscription struct {
	names []string
	ended bool // the listener has left for good: it joins nothing more
}

var topics = struct {
	sync.RWMutex
	listeners map[string]map[listener]struct{}
}{listeners: map[string]map[listener]struct{}{}}

func checkTopic(name string) error {
	if name == "" || len(name) > MaxTopicName {
		return fmt.Errorf("a topic's name has 1 to %d bytes", MaxTopicName)
	}
	return nil
}

// join makes l a listener of names, in addition to what sub has: of all
// of them or, with an error, of none.
func join(l listener, sub *subscription, names []string) error {
	for _, name := range names {
		if err := checkTopic(name); err != nil {
			return err
		}
	}
	topics.Lock()
	defer topics.Unlock()
	if sub.ended {
		return ErrStreamClosed
	}
	added := make([]string, 0, len(names))
	for _, name := range names {
		if _, has := topics.listeners[name][l]; !has && !slices.Contains(added, name) {
			added = append(added, name)
		}
	}
	if len(sub.names)+len(added) > maxTopics {
		return fmt.Errorf("a client listens to %d topics at most", maxTopics)
	}
	for _, name := range added {
		set := topics.listeners[name]
		if set == nil {
			set = map[listener]struct{}{}
			topics.listeners[name] = set
		}
		set[l] = struct{}{}
	}
	sub.names = append(sub.names, added...)
	return nil
}

// leave ends l's listening to names, or for good when names is nil.
func leave(l listener, sub *subscription, names []string) {
	topics.Lock()
	defer topics.Unlock()
	if names == nil {
		names, sub.names, sub.ended = sub.names, nil, true
	}
	for _, name := range names {
		set := topics.listeners[name]
		if _, has := set[l]; !has {
			continue
		}
		delete(set, l)
		if len(set) == 0 {
			delete(topics.listeners, name)
		}
		for i, kept := range sub.names {
			if kept == name {
				sub.names = append(sub.names[:i], sub.names[i+1:]...)
				break
			}
		}
	}
}

// publication is one message on its way to the listeners of a topic. Each
// kind of listener gets it in its own form, made when the first one asks.
type publication struct {
	data      []byte
	event, id string
	message   []byte // for a WebSocket: a text message
	sse       []byte // for Server-Sent Events
}

func (m *publication) asMessage() []byte {
	if m.message == nil {
		m.message = Message(false, m.data)
	}
	return m.message
}

func (m *publication) asEvent() []byte {
	if m.sse != nil {
		return m.sse
	}
	var b bytes.Buffer
	b.Grow(len(m.data) + len(m.event) + len(m.id) + 32)
	if m.event != "" {
		b.WriteString("event: " + m.event + "\n")
	}
	if m.id != "" {
		b.WriteString("id: " + m.id + "\n")
	}
	// Every line of the data is a field of its own.
	data := strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(string(m.data))
	for _, line := range strings.Split(data, "\n") {
		b.WriteString("data: " + line + "\n")
	}
	b.WriteByte('\n')
	m.sse = b.Bytes()
	return m.sse
}

// published is a message that waits to be handed out.
type published struct {
	topic string
	m     *publication
}

// The messages on their way to the listeners. The goroutine that hands
// them out starts with the first one and lives as long as the process, as
// the topics do.
var (
	handOut     chan published
	handOutOnce sync.Once
)

func publish(topic string, data []byte, event, id string) (published, error) {
	if err := checkTopic(topic); err != nil {
		return published{}, err
	}
	if len(data) > MaxPublication {
		return published{}, fmt.Errorf("a published message has %d bytes at most", MaxPublication)
	}
	if len(event) > MaxTopicName || len(id) > MaxTopicName ||
		strings.ContainsAny(event, "\r\n") || strings.ContainsAny(id, "\r\n") {
		return published{}, fmt.Errorf("an event's name and id are one line of %d bytes at most", MaxTopicName)
	}
	handOutOnce.Do(func() {
		handOut = make(chan published, publishQueue)
		go func() {
			for p := range handOut {
				p.hand()
			}
		}()
	})
	return published{topic, &publication{data: data, event: event, id: id}}, nil
}

// hand gives the message to every listener of its topic, waiting for none.
func (p published) hand() {
	var slow []listener
	topics.RLock()
	for l := range topics.listeners[p.topic] {
		if !l.deliver(p.m) {
			slow = append(slow, l)
		}
	}
	topics.RUnlock()
	for _, l := range slow {
		l.drop()
	}
}

func listeners(topic string) int {
	topics.RLock()
	defer topics.RUnlock()
	return len(topics.listeners[topic])
}

// TryPublish queues data for every listener of topic, without waiting, and
// returns how many listen now. queued is false when the queue is full, and
// the caller should use Publish. data is kept: the caller gives it away.
// event and id are for the Server-Sent Events listeners: the event's name
// and its id, a line each.
func TryPublish(topic string, data []byte, event, id string) (listening int, queued bool, err error) {
	p, err := publish(topic, data, event, id)
	if err != nil {
		return 0, false, err
	}
	select {
	case handOut <- p:
		return listeners(topic), true, nil
	default:
		return 0, false, nil
	}
}

// Publish is TryPublish that waits for room in the queue until ctx ends.
func Publish(ctx context.Context, topic string, data []byte, event, id string) (int, error) {
	p, err := publish(topic, data, event, id)
	if err != nil {
		return 0, err
	}
	select {
	case handOut <- p:
		return listeners(topic), nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// SubscribeSocket makes socket id a listener of names: what is published
// there goes to its client as a text message, through the queue that PHP
// sends into.
func SubscribeSocket(id int64, names []string) error {
	s, err := socketOf(id)
	if err != nil {
		return err
	}
	return join(s, &s.subscription, names)
}

// UnsubscribeSocket ends that for names.
func UnsubscribeSocket(id int64, names []string) {
	if s, err := socketOf(id); err == nil && len(names) > 0 {
		leave(s, &s.subscription, names)
	}
}

func (s *socket) deliver(m *publication) bool {
	queued, err := s.sends.TryPut(m.asMessage())
	return queued && err == nil
}

// drop cuts the connection: the reader ends, and with it the handler.
func (s *socket) drop() { s.leave() }

func (s *socket) unsubscribe() { leave(s, &s.subscription, nil) }

// topicsAnswer is the answer of a request that PHP answered with topics
// (ListenTopics): Server-Sent Events that Go serves until the client
// leaves. Its handler in Go is all that the client holds.
type topicsAnswer struct {
	ev        *event
	header    map[string][]string
	keepAlive time.Duration
	subscription
	st   *stream
	end  chan struct{} // closed when the listener is to end
	once sync.Once
	slow bool // it ends because the client did not keep up
}

// ListenTopics makes the answer of the request that ctx is an operation of
// the events of names, with header in its head. Go begins it once PHP has
// answered the call; a comment every keepAlive keeps the connection from
// being closed as idle (0 for none).
func ListenTopics(ctx context.Context, names []string, header map[string][]string, keepAlive time.Duration) error {
	if len(names) == 0 || len(names) > maxTopics {
		return fmt.Errorf("an answer listens to 1 to %d topics", maxTopics)
	}
	for _, name := range names {
		if err := checkTopic(name); err != nil {
			return err
		}
	}
	ev, err := writerOf(ctx)
	if err != nil {
		return err
	}
	defer ev.mu.Unlock()
	if ev.began != nil {
		return errors.New("the answer has already begun")
	}
	t := &topicsAnswer{ev: ev, header: header, keepAlive: keepAlive, end: make(chan struct{})}
	t.names = names // joined when the answer begins
	ev.began = t
	return nil
}

func (t *topicsAnswer) deliver(m *publication) bool {
	queued, err := t.st.sends.TryPut(m.asEvent())
	return queued && err == nil
}

func (t *topicsAnswer) drop() {
	t.once.Do(func() {
		t.slow = true
		close(t.end)
	})
}

// stop ends the answer as one that is complete: the server is stopping.
func (t *topicsAnswer) stop() { t.once.Do(func() { close(t.end) }) }

// finish serves the events, on the goroutine of the request's handler.
func (t *topicsAnswer) finish(failed bool) {
	if failed {
		http.Error(t.ev.w, "internal server error", http.StatusInternalServerError)
		return
	}
	names := t.names
	t.names = nil
	t.st = t.ev.openStream(http.StatusOK, t.header)
	if err := join(t, &t.subscription, names); err != nil {
		panic(http.ErrAbortHandler) // ListenTopics checked them: not reached
	}
	t.listen()
	leave(t, &t.subscription, nil)
	if t.slow {
		// The writer may be held by the client: it ends with the connection.
		go func() { _ = t.st.sends.Close() }()
		panic(http.ErrAbortHandler)
	}
	if t.st.sends.Close() != nil {
		panic(http.ErrAbortHandler)
	}
}

func (t *topicsAnswer) listen() {
	var tick <-chan time.Time
	if t.keepAlive > 0 {
		ticker := time.NewTicker(t.keepAlive)
		defer ticker.Stop()
		tick = ticker.C
	}
	for {
		select {
		case <-t.ev.Done(): // the client left
			return
		case <-t.end:
			return
		case <-tick:
			if _, err := t.st.sends.TryPut([]byte(topicsKeepAlive)); err != nil {
				return
			}
		}
	}
}

// stopTopics runs when the server begins to stop: the answers that Go
// serves end, and their clients connect again elsewhere.
func stopTopics() {
	var answers []*topicsAnswer
	topics.RLock()
	for _, set := range topics.listeners {
		for l := range set {
			if t, ok := l.(*topicsAnswer); ok {
				answers = append(answers, t)
			}
		}
	}
	topics.RUnlock()
	for _, t := range answers {
		t.stop()
	}
}

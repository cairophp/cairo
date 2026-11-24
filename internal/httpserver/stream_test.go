package httpserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// PHP may still run when the handler of its request has returned: the
// client left. Nothing of it writes to the handler's ResponseWriter then.
func TestNoWriteAfterTheHandlerReturned(t *testing.T) {
	w := httptest.NewRecorder()
	ev := &event{Context: context.Background(), w: w}
	if err := Hint(ev, http.StatusEarlyHints, map[string][]string{"Link": {"</a.css>; rel=preload"}}); err != nil {
		t.Fatalf("a hint while the handler runs: %v", err)
	}
	if st := ev.end(); st != nil {
		t.Fatalf("a stream that nobody opened: %v", st)
	}
	if _, err := OpenStream(ev, 200, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("a stream opened after the handler returned: %v", err)
	}
	if err := Hint(ev, http.StatusEarlyHints, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("a hint was sent after the handler returned: %v", err)
	}
	if _, err := OpenStream(context.Background(), 200, nil); err == nil {
		t.Error("a stream opened for a context that is no request's")
	}

	ev = &event{Context: context.Background(), w: httptest.NewRecorder()}
	id, err := OpenStream(ev, 200, map[string][]string{"content-type": {"text/plain"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStream(ev, 200, nil); err == nil {
		t.Error("an answer began twice")
	}
	if queued, err := TrySend(id, []byte("piece")); !queued || err != nil {
		t.Errorf("a piece was not queued: %v, %v", queued, err)
	}
	ev.end().finish(false)
	if got := ev.w.(*httptest.ResponseRecorder); got.Body.String() != "piece" || got.Header().Get("Content-Type") != "text/plain" {
		t.Errorf("the client got %q with %v", got.Body, got.Header())
	}
	if _, err := TrySend(id, []byte("late")); !errors.Is(err, ErrStreamClosed) {
		t.Errorf("a piece after the stream's end: %v", err)
	}
}

//go:build !nomercure

package mercurehub

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

// The hub answers its own paths and no other, hands what is published to
// its subscribers, and is gone from the process once it is stopped.
func TestHubServesPublishesAndStops(t *testing.T) {
	d := Declaration{Anonymous: true, Heartbeat: 40, WriteTimeout: 600, DispatchTimeout: 5}
	hub, err := Start(d, quiet(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(hub.Stop)
	if _, err := Start(d, quiet(), nil); err == nil || !strings.Contains(err.Error(), "one hub") {
		t.Errorf("a second hub: %v", err)
	}
	srv := httptest.NewServer(hub.Routes(http.NotFoundHandler()))
	t.Cleanup(srv.Close)
	if resp, err := http.Get(srv.URL + "/hello"); err != nil || resp.StatusCode != 404 {
		t.Errorf("a path that is not the hub's: %v, %v", resp, err)
	}

	resp, err := http.Get(srv.URL + Path + "?match=news")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("subscribe: %v, %v", resp, err)
	}
	defer resp.Body.Close()
	id, err := Publish(context.Background(), Update{Topics: []string{"news"}, Data: "hello", Type: "greeting"})
	if err != nil || id == "" {
		t.Fatalf("Publish: %q, %v", id, err)
	}
	var event []string
	for lines := bufio.NewScanner(resp.Body); lines.Scan() && lines.Text() != ""; {
		// The hub opens a stream with a comment.
		if !strings.HasPrefix(lines.Text(), ":") {
			event = append(event, lines.Text())
		}
	}
	if got, want := strings.Join(event, "|"), "event: greeting|id: "+id+"|data: hello"; got != want {
		t.Errorf("event = %q, want %q", got, want)
	}

	_, err = Publish(context.Background(), Update{Topics: []string{"*"}})
	if !Invalid(err) {
		t.Errorf("an update to the wildcard: %v, want one that Invalid reports", err)
	}
	hub.Stop()
	if _, err := Publish(context.Background(), Update{Topics: []string{"news"}}); !errors.Is(err, ErrNoHub) || Invalid(err) {
		t.Errorf("Publish after Stop: %v, want ErrNoHub", err)
	}
	// The process may have a hub again.
	again, err := Start(d, quiet(), nil)
	if err != nil {
		t.Fatal(err)
	}
	again.Stop()
}

// Without the hub's tags for it, compatibility() is refused instead of
// silently doing nothing; with them the hub takes it.
func TestCompatibilityNeedsItsTags(t *testing.T) {
	d := Declaration{Anonymous: true, Compatibility: 8, Heartbeat: 40, WriteTimeout: 600, DispatchTimeout: 5}
	hub, err := Start(d, quiet(), nil)
	if compatible != (err == nil) {
		t.Fatalf("built with the tags: %v, Start: %v", compatible, err)
	}
	hub.Stop()
}

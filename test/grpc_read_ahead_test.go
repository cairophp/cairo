package test

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/dynamicpb"
)

// A client that sends faster than the handler reads: Go reads its messages
// ahead of PHP up to a bound, and the handler gets every one, in order.
func TestGRPCHandlerReadsAFastClient(t *testing.T) {
	g := newGreeter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const n = 20000
	cs, md := g.stream(ctx, t, "JoinNames")
	names := make([]string, n)
	for i := range names {
		names[i] = strconv.Itoa(i)
		if err := cs.SendMsg(msg(md.Input(), map[string]any{"name": names[i]})); err != nil {
			t.Fatal(err)
		}
	}
	_ = cs.CloseSend()
	out := dynamicpb.NewMessage(md.Output())
	if err := cs.RecvMsg(out); err != nil {
		t.Fatal(err)
	}
	if got := replyText(out); got != "Hello "+strings.Join(names, ", ") {
		t.Fatalf("JoinNames of %d names answered %d bytes that begin %.60q", n, len(got), got)
	}
}

// PHP's client reads a stream more slowly than its replies arrive: none is
// lost, their order holds, and the status that ends a stream comes after
// the replies that arrived before it.
func TestGRPCClientReadsAhead(t *testing.T) {
	const n = 20000
	raw := get(t, "/grpc-stream/ahead?"+url.Values{"target": {grpcAddr}, "count": {strconv.Itoa(n)}}.Encode())
	var out struct {
		Read    int      `json:"read"`
		Ordered bool     `json:"ordered"`
		Chat    []string `json:"chat"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	if out.Read != n || !out.Ordered || strings.Join(out.Chat, ",") != "Hello a,Hello b,status 3" {
		t.Fatalf("got %s", strings.TrimSpace(raw))
	}
}

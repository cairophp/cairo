package test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// stream opens a streaming call to the PHP Greeter with a Go client.
func (g *greeter) stream(ctx context.Context, t *testing.T, method string) (grpc.ClientStream, protoreflect.MethodDescriptor) {
	t.Helper()
	md := g.svc.Methods().ByName(protoreflect.Name(method))
	cs, err := g.conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: md.IsStreamingServer(), ClientStreams: md.IsStreamingClient()}, "/helloworld.Greeter/"+method)
	if err != nil {
		t.Fatal(err)
	}
	return cs, md
}

func msg(md protoreflect.MessageDescriptor, fields map[string]any) *dynamicpb.Message {
	m := dynamicpb.NewMessage(md)
	for k, v := range fields {
		m.Set(md.Fields().ByName(protoreflect.Name(k)), protoreflect.ValueOf(v))
	}
	return m
}

func replyText(m *dynamicpb.Message) string {
	return m.Get(m.Descriptor().Fields().ByName("message")).String()
}

// PHP streaming handlers, called by a Go client: server, client and
// bidirectional streaming, and a handler's status ending a stream.
func TestGRPCStreamingHandlers(t *testing.T) {
	g := newGreeter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cs, md := g.stream(ctx, t, "CountUp")
	if err := cs.SendMsg(msg(md.Input(), map[string]any{"name": "go", "count": int32(3)})); err != nil {
		t.Fatal(err)
	}
	_ = cs.CloseSend()
	var got []string
	for {
		out := dynamicpb.NewMessage(md.Output())
		if err := cs.RecvMsg(out); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		got = append(got, replyText(out))
	}
	if strings.Join(got, ",") != "go 1,go 2,go 3" {
		t.Fatalf("CountUp: %v", got)
	}

	cs, md = g.stream(ctx, t, "JoinNames")
	for _, n := range []string{"a", "b"} {
		if err := cs.SendMsg(msg(md.Input(), map[string]any{"name": n})); err != nil {
			t.Fatal(err)
		}
	}
	_ = cs.CloseSend()
	out := dynamicpb.NewMessage(md.Output())
	if err := cs.RecvMsg(out); err != nil || replyText(out) != "Hello a, b" {
		t.Fatalf("JoinNames: %v %q", err, replyText(out))
	}

	cs, md = g.stream(ctx, t, "Chat")
	for _, n := range []string{"x", "y"} {
		if err := cs.SendMsg(msg(md.Input(), map[string]any{"name": n})); err != nil {
			t.Fatal(err)
		}
		out := dynamicpb.NewMessage(md.Output())
		if err := cs.RecvMsg(out); err != nil || replyText(out) != "Hello "+n {
			t.Fatalf("Chat %s: %v %q", n, err, replyText(out))
		}
	}
	if err := cs.SendMsg(msg(md.Input(), map[string]any{"name": ""})); err != nil {
		t.Fatal(err)
	}
	err := cs.RecvMsg(dynamicpb.NewMessage(md.Output()))
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("Chat with an empty name: %v", err)
	}
}

// PHP streaming calls through the generated client, against the same
// server's PHP handlers.
func TestGRPCStreamingClient(t *testing.T) {
	raw := get(t, "/grpc-stream/all?"+url.Values{"target": {grpcAddr}}.Encode())
	var out struct {
		CountUp     []string `json:"count_up"`
		Join        string   `json:"join"`
		Chat        []string `json:"chat"`
		ChatEnd     any      `json:"chat_end"`
		HasTrailers bool     `json:"has_trailers"`
		Error       string   `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	if strings.Join(out.CountUp, ",") != "n 1,n 2,n 3" || out.Join != "Hello Ada, Grace, Linus" ||
		strings.Join(out.Chat, ",") != "Hello one,Hello two" || out.ChatEnd != nil || !out.HasTrailers || out.Error != "invalid_argument" {
		t.Fatalf("got %s", strings.TrimSpace(raw))
	}
}

// 50 requests each read a server stream that takes 1s, served by handlers
// on the same single PHP thread.
func TestGRPCStreamingDoesNotBlockTheThread(t *testing.T) {
	const n = 50
	q := url.Values{"target": {grpcAddr}}.Encode()
	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get("http://" + httpAddr + "/grpc-stream/slow?" + q)
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(b), `"messages":["slow 1","slow 2","slow 3"]`) {
				errs <- fmt.Errorf("status %d body %q", resp.StatusCode, b)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	took := time.Since(start)
	t.Logf("%d streaming calls of 1s (3 messages each), on 1 PHP thread: %s", n, took.Round(time.Millisecond))
	if took > 3*time.Second {
		t.Fatalf("took %s; streaming blocked the thread", took)
	}
}

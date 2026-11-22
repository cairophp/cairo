package httpserver

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/cairophp/cairo/internal/wire"
)

// The event leaves its body out and says how long it is: with the body put
// behind it, as the engine does, it is one well-formed map.
func TestEventWithItsTailDecodes(t *testing.T) {
	for _, body := range [][]byte{nil, []byte("x"), bytes.Repeat([]byte{0, 0xff, '"'}, 50_000)} {
		ev := &event{route: 3, method: "POST", path: "/p", headers: http.Header{"A": {"b"}}, body: body, host: "h",
			values: map[string]any{"request_id": "r1"}, form: &multipartForm{fields: []formField{{"f", "v"}}}}
		var got struct {
			Route  int               `json:"route"`
			Method string            `json:"method"`
			Host   string            `json:"host"`
			Values map[string]string `json:"values"`
			Body   []byte            `json:"body"`
		}
		if err := wire.Unmarshal(append(ev.AppendWire(nil, 7, 1.5), ev.Tail()...), &got); err != nil {
			t.Fatalf("body of %d bytes: %v", len(body), err)
		}
		if got.Route != 3 || got.Method != "POST" || got.Host != "h" || got.Values["request_id"] != "r1" || !bytes.Equal(got.Body, body) {
			t.Fatalf("body of %d bytes: decoded route %d, %s, host %q, values %v, %d bytes of body", len(body), got.Route, got.Method, got.Host, got.Values, len(got.Body))
		}
	}
}

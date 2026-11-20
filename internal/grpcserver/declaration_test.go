package grpcserver

import (
	"testing"

	"github.com/cairophp/cairo/internal/spec"
)

func TestDeclared(t *testing.T) {
	cases := map[string]struct {
		grpc string
		n    int // servers read; 0 when the declaration is refused
	}{
		"services":                {`[{"listen":":9000","protos":["a.proto"],"services":[{"id":0,"name":"x.Y"},{"id":1,"name":"x.Z"}]}]`, 1},
		"no services":             {`[{"listen":":9000"}]`, 1},
		"empty listen":            {`[{"listen":""}]`, 0},
		"same listen":             {`[{"listen":":9000"},{"listen":":9000"}]`, 0},
		"services without protos": {`[{"listen":":9000","services":[{"id":0,"name":"x.Y"}]}]`, 0},
		"duplicate handler id":    {`[{"listen":":9000","protos":["a.proto"],"services":[{"id":0,"name":"x.Y"},{"id":0,"name":"x.Z"}]}]`, 0},
	}
	for name, c := range cases {
		s, err := spec.Parse([]byte(`{"extensions":{"grpc":` + c.grpc + `}}`))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		decls, err := Declared(s, &spec.Claims{})
		if len(decls) != c.n || (err == nil) != (c.n > 0) {
			t.Errorf("%s: %d servers, err = %v, want %d", name, len(decls), err, c.n)
		}
	}
}

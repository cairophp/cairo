package main

import (
	"testing"

	"github.com/cairophp/cairo/internal/spec"
)

func TestDeclarationsRead(t *testing.T) {
	cases := map[string]struct {
		extensions string
		ok         bool
	}{
		"http and grpc":           {`{"http":[{"listen":":8080","routes":[{"id":0,"method":"GET","path":"/a"}]}],"grpc":[{"listen":":9000","protos":["a.proto"],"services":[{"id":1,"name":"x.Y"}]}]}`, true},
		"http only":               {`{"http":[{"listen":":8080"}],"grpc":[]}`, true},
		"no servers":              {`{"http":[],"grpc":[]}`, false},
		"same listen on both":     {`{"http":[{"listen":":8080"}],"grpc":[{"listen":":8080"}]}`, false},
		"same handler id on both": {`{"http":[{"listen":":8080","routes":[{"id":0,"method":"GET","path":"/a"}]}],"grpc":[{"listen":":9000","protos":["a.proto"],"services":[{"id":0,"name":"x.Y"}]}]}`, false},
	}
	for name, c := range cases {
		s, err := spec.Parse([]byte(`{"extensions":` + c.extensions + `}`))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var d declarations
		if err := d.read(s); (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok=%v", name, err, c.ok)
		}
	}
}

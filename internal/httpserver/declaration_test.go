package httpserver

import (
	"testing"

	"github.com/cairophp/cairo/internal/spec"
)

func TestDeclared(t *testing.T) {
	cases := map[string]struct {
		http string
		n    int // servers read; 0 when the declaration is refused
	}{
		"routes":                  {`[{"listen":":8080","routes":[{"id":0,"method":"GET","path":"/a"}]},{"listen":":8081"}]`, 2},
		"empty listen":            {`[{"listen":""}]`, 0},
		"same listen":             {`[{"listen":":8080"},{"listen":":8080"}]`, 0},
		"duplicate route":         {`[{"listen":":8080","routes":[{"id":0,"method":"GET","path":"/a"},{"id":1,"method":"get","path":"/a"}]}]`, 0},
		"duplicate handler id":    {`[{"listen":":8080","routes":[{"id":0,"method":"GET","path":"/a"},{"id":0,"method":"GET","path":"/b"}]}]`, 0},
		"relative path":           {`[{"listen":":8080","routes":[{"id":0,"method":"GET","path":"a"}]}]`, 0},
		"cached route":            {`[{"listen":":8080","routes":[{"id":0,"method":"GET","path":"/a","cache":{"ttl":5,"vary":["Accept-Language"],"tags":["products"]}}]}]`, 1},
		"cache without ttl":       {`[{"listen":":8080","routes":[{"id":0,"method":"GET","path":"/a","cache":{"ttl":0}}]}]`, 0},
		"cache vary not a header": {`[{"listen":":8080","routes":[{"id":0,"method":"GET","path":"/a","cache":{"ttl":5,"vary":["a b"]}}]}]`, 0},
		"empty cache tag":         {`[{"listen":":8080","routes":[{"id":0,"method":"GET","path":"/a","cache":{"ttl":5,"tags":[""]}}]}]`, 0},
		"metrics and probes":      {`[{"listen":":9100","metrics":"/metrics","live":"/healthz","ready":"/readyz"}]`, 1},
		"metrics on a route":      {`[{"listen":":8080","routes":[{"id":0,"method":"GET","path":"/metrics"}],"metrics":"/metrics"}]`, 0},
		"probes on one path":      {`[{"listen":":8080","live":"/health","ready":"/health"}]`, 0},
		"metrics path a pattern":  {`[{"listen":":8080","metrics":"/metrics/{any}"}]`, 0},
		"relative metrics path":   {`[{"listen":":8080","metrics":"metrics"}]`, 0},
	}
	for name, c := range cases {
		s, err := spec.Parse([]byte(`{"extensions":{"http":` + c.http + `}}`))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		decls, err := Declared(s, &spec.Claims{})
		if len(decls) != c.n || (err == nil) != (c.n > 0) {
			t.Errorf("%s: %d servers, err = %v, want %d", name, len(decls), err, c.n)
		}
	}
}

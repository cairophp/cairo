package spec

import "testing"

func TestValidate(t *testing.T) {
	cases := map[string]struct {
		json string
		ok   bool
	}{
		"http and grpc":           {`{"http":[{"listen":":8080","routes":[{"id":0,"method":"GET","path":"/a"}]}],"grpc":[{"listen":":9000","protos":["a.proto"],"services":[{"id":1,"name":"x.Y"}]}]}`, true},
		"no servers":              {`{}`, false},
		"same listen":             {`{"http":[{"listen":":8080"},{"listen":":8080"}]}`, false},
		"duplicate route":         {`{"http":[{"listen":":8080","routes":[{"id":0,"method":"GET","path":"/a"},{"id":1,"method":"get","path":"/a"}]}]}`, false},
		"relative path":           {`{"http":[{"listen":":8080","routes":[{"id":0,"method":"GET","path":"a"}]}]}`, false},
		"services without protos": {`{"grpc":[{"listen":":9000","services":[{"id":0,"name":"x.Y"}]}]}`, false},
	}
	for name, c := range cases {
		_, err := Parse([]byte(c.json))
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok=%v", name, err, c.ok)
		}
	}
}

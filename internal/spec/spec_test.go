package spec

import "testing"

func TestParse(t *testing.T) {
	cases := map[string]struct {
		json string
		ok   bool
	}{
		"nothing declared":   {`{}`, true},
		"core settings":      {`{"threads":4,"offload_threads":2,"max_in_flight":100,"isolated":true}`, true},
		"negative threads":   {`{"threads":-1}`, false},
		"negative in flight": {`{"max_in_flight":-1}`, false},
		"not an object":      {`[]`, false},
	}
	for name, c := range cases {
		_, err := Parse([]byte(c.json))
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok=%v", name, err, c.ok)
		}
	}
}

func TestSection(t *testing.T) {
	s, err := Parse([]byte(`{"extensions":{"http":[{"listen":":8080"}],"grpc":7}}`))
	if err != nil {
		t.Fatal(err)
	}
	var servers []struct {
		Listen string `json:"listen"`
	}
	if err := s.Section("http", &servers); err != nil || len(servers) != 1 || servers[0].Listen != ":8080" {
		t.Errorf("http: got %v, err = %v", servers, err)
	}
	if err := s.Section("grpc", &servers); err == nil {
		t.Error("grpc: a number was read as a list of servers")
	}
	// An extension that declared nothing leaves the value as it was.
	if err := s.Section("other", &servers); err != nil || len(servers) != 1 {
		t.Errorf("other: got %v, err = %v", servers, err)
	}
}

func TestClaims(t *testing.T) {
	var c Claims
	steps := []struct {
		name string
		err  error
		ok   bool
	}{
		{"a listen address", c.Listen(":8080"), true},
		{"another one", c.Listen(":9000"), true},
		{"the first again", c.Listen(":8080"), false},
		{"an empty one", c.Listen(""), false},
		{"a handler id", c.Handler(0), true},
		{"another one", c.Handler(1), true},
		{"the first again", c.Handler(0), false},
	}
	for _, s := range steps {
		if (s.err == nil) != s.ok {
			t.Errorf("%s: err = %v, want ok=%v", s.name, s.err, s.ok)
		}
	}
}

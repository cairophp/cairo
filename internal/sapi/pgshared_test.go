package sapi

import "testing"

func TestPgSharedOption(t *testing.T) {
	for _, c := range []struct {
		in, rest string
		n        int
		bad      bool
	}{
		{in: "host=db;dbname=app", rest: "host=db;dbname=app"},
		{in: "host=db;cairo_pool=40;dbname=app", rest: "host=db;dbname=app", n: 40},
		{in: "host=db; cairo_pool = 8", rest: "host=db", n: 8},
		{in: "host=db;cairo_pool=0", bad: true},
		{in: "host=db;cairo_pool=x", bad: true},
		{in: "postgres://u@db/app?cairo_pool=5&sslmode=disable", rest: "postgres://u@db/app?sslmode=disable", n: 5},
		{in: "postgres://u@db/app", rest: "postgres://u@db/app"},
	} {
		rest, n, err := pgSharedOption(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("%q: want an error", c.in)
			}
			continue
		}
		if err != nil || rest != c.rest || n != c.n {
			t.Errorf("%q: got %q, %d, %v; want %q, %d", c.in, rest, n, err, c.rest, c.n)
		}
	}
}

package test

import (
	"strings"
	"testing"
)

// The parameters of a prepared statement reach PostgreSQL as PHP has them:
// ” is not NULL, and text and bytes are not changed on the way. In a
// request's fiber, and on an offload thread, which waits for Go.
func TestPDOPgsqlParameters(t *testing.T) {
	pgsqlEnv(t)
	const want = `{"empty":"","null_is_null":true,"text":true,"bytes":true,"no_bytes":0}`
	for _, path := range []string{"/pgsql/params", "/pgsql/params?offload=1"} {
		if got := strings.TrimSpace(get(t, path)); got != want {
			t.Errorf("%s: got %s", path, got)
		}
	}
}

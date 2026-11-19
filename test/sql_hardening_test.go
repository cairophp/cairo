package test

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

// examples/sql/hardening.php on PostgreSQL and MySQL: statements that would
// leave a transaction on a pooled connection are refused or cleaned up, and
// acquire_timeout bounds the wait for a connection.
func TestSqlHardening(t *testing.T) {
	for name, dsn := range map[string]func(*testing.T) string{"pgsql": sqlDSN, "mysql": mysqlURL} {
		t.Run(name, func(t *testing.T) {
			do := startSQLScript(t, "hardening.php", dsn(t), "1")
			for _, sql := range []string{"BEGIN", "/* x */ start transaction", "SET autocommit = 0", "LOCK TABLES t WRITE"} {
				if code, body := do("GET", "/begin?sql="+url.QueryEscape(sql)); code != 200 || !strings.HasPrefix(body, "refused: ") {
					t.Errorf("%q on the pool: %d %s, want it refused", sql, code, body)
				}
			}
			if code, body := do("GET", "/begin?sql=SELECT+1"); code != 200 || body != "ran" {
				t.Errorf("SELECT 1: %d %s", code, body)
			}
			if code, body := do("GET", "/sneak"); code != 200 || body != map[string]string{"pgsql": "ran", "mysql": "refused"}[name] {
				t.Errorf("/sneak: %d %s", code, body)
			}
			if code, body := do("GET", "/in-transaction"); code != 200 || body != "false" {
				t.Errorf("/in-transaction: %d %s, want the next request outside any transaction", code, body)
			}

			// The holder and the waiter run on the same PHP thread: the
			// waiter gives up after 0.1 s while the holder sleeps 0.5 s.
			var res struct {
				Error string  `json:"error"`
				Took  float64 `json:"took"`
				After int     `json:"after"`
			}
			code, body := do("GET", "/acquire-timeout")
			if err := json.Unmarshal([]byte(body), &res); code != 200 || err != nil ||
				!strings.HasPrefix(res.Error, "no connection became free in 100ms") || res.Took < 0.09 || res.Took > 0.3 || res.After != 1 {
				t.Errorf("/acquire-timeout: %d %s", code, body)
			}
		})
	}
}

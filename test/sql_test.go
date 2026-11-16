package test

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

// sqlDSN turns CAIRO_TEST_PGSQL (a PDO DSN) and its user and password into
// a key=value DSN for Cairo\Sql.
func sqlDSN(t *testing.T) string {
	pgsqlEnv(t)
	dsn := strings.ReplaceAll(strings.TrimPrefix(os.Getenv("CAIRO_TEST_PGSQL"), "pgsql:"), ";", " ")
	if u := os.Getenv("CAIRO_TEST_PGSQL_USER"); u != "" {
		dsn += " user=" + u
	}
	if p := os.Getenv("CAIRO_TEST_PGSQL_PASSWORD"); p != "" {
		dsn += " password=" + p
	}
	return dsn + " sslmode=disable"
}

// mysqlURL turns CAIRO_TEST_MYSQL (a PDO DSN) and its user and password into
// a mysql:// URL for Cairo\Sql, and creates its database, cairo_sql.
func mysqlURL(t *testing.T) string {
	pdo := os.Getenv("CAIRO_TEST_MYSQL")
	if pdo == "" {
		t.Skip("CAIRO_TEST_MYSQL is not set")
	}
	host, port := "127.0.0.1", "3306"
	for _, kv := range strings.Split(strings.TrimPrefix(pdo, "mysql:"), ";") {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "host":
			host = v
		case "port":
			port = v
		}
	}
	cfg := mysql.NewConfig()
	cfg.User, cfg.Passwd = os.Getenv("CAIRO_TEST_MYSQL_USER"), os.Getenv("CAIRO_TEST_MYSQL_PASSWORD")
	cfg.Net, cfg.Addr = "tcp", host+":"+port
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE DATABASE IF NOT EXISTS cairo_sql"); err != nil {
		t.Fatal(err)
	}
	u := url.URL{Scheme: "mysql", User: url.UserPassword(cfg.User, cfg.Passwd), Host: cfg.Addr, Path: "/cairo_sql"}
	return u.String()
}

// startSQL runs examples/sql on one PHP thread, against PostgreSQL.
func startSQL(t *testing.T, maxOpen string) func(method, path string) (int, string) {
	return startSQLScript(t, "server.php", sqlDSN(t), maxOpen)
}

// startSQLScript runs a script of examples/sql on one PHP thread.
func startSQLScript(t *testing.T, script, dsn, maxOpen string) func(method, path string) (int, string) {
	t.Helper()
	addr := freeAddr()
	cmd := exec.Command(cairoBin, "serve", "../examples/sql/"+script, "--threads", "1", "--offload-threads", "1")
	cmd.Env = append(os.Environ(), "CAIRO_HTTP="+addr, "CAIRO_SQL_DSN="+dsn, "CAIRO_SQL_MAX_OPEN="+maxOpen)
	var stderr syncBuffer
	cmd.Stdout, cmd.Stderr = os.Stderr, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("server log:\n%s", stderr.String())
		}
	})
	if err := waitFor(addr, 10*time.Second); err != nil {
		t.Fatalf("%v\n%s", err, stderr.String())
	}
	client := &http.Client{Timeout: 10 * time.Second}
	return func(method, path string) (int, string) {
		req, _ := http.NewRequest(method, "http://"+addr+path, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Errorf("%s %s: %v", method, path, err)
			return 0, ""
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, strings.TrimSpace(string(b))
	}
}

func TestSqlQueries(t *testing.T) {
	do := startSQL(t, "10")
	if code, body := do("POST", "/reset"); code != 200 {
		t.Fatalf("/reset: %d %s", code, body)
	}

	if code, body := do("POST", "/items?name=first"); code != 200 || body != `{"inserted":1}` {
		t.Errorf("insert: %d %s", code, body)
	}
	if code, body := do("GET", "/items"); code != 200 || body != `[{"id":1,"name":"first"}]` {
		t.Errorf("select: %d %s", code, body)
	}

	want := `{"int":42,"float":1.5,"bool":true,"text":"héllo","null":null,"bytes":"00ff10","time":"2026-01-02T03:04:05Z","numeric":"12.50","json":"{\"a\": 1}"}`
	if code, body := do("GET", "/types"); code != 200 || body != want {
		t.Errorf("/types: %d\n got %s\nwant %s", code, body, want)
	}
	if code, body := do("GET", "/params"); code != 200 || body != `{"b":"00ff10","t":"2026-05-06T07:08:09Z","n":9007199254740994}` {
		t.Errorf("/params: %d %s", code, body)
	}

	if code, body := do("POST", "/tx?fail=1"); code != 200 || body != `{"count":1}` {
		t.Errorf("rolled-back transaction: %d %s, want the count unchanged", code, body)
	}
	if code, body := do("POST", "/tx"); code != 200 || body != `{"count":3}` {
		t.Errorf("committed transaction: %d %s, want 2 more rows", code, body)
	}

	for kind, state := range map[string]string{"table": "42P01", "unique": "23505"} {
		code, body := do("GET", "/error?kind="+kind)
		var e struct {
			SQLState string `json:"sqlstate"`
		}
		if err := json.Unmarshal([]byte(body), &e); code != 200 || err != nil || e.SQLState != state {
			t.Errorf("/error?kind=%s: %d %s, want SQLSTATE %s", kind, code, body, state)
		}
	}
}

// 50 requests each running pg_sleep(1) on one PHP thread, with a pool of 50
// connections, take about 1s: the queries wait in Go.
func TestSqlConcurrency(t *testing.T) {
	do := startSQL(t, "50")
	start := time.Now()
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if code, body := do("GET", "/sleep"); code != 200 || body != "1" {
				t.Errorf("/sleep: %d %s", code, body)
			}
		}()
	}
	wg.Wait()
	took := time.Since(start)
	if took > 3*time.Second {
		t.Fatalf("50 pg_sleep(1) queries on 1 PHP thread took %s", took)
	}
	var stats struct {
		MaxOpen int `json:"max_open"`
		Open    int `json:"open"`
	}
	_, body := do("GET", "/stats")
	if err := json.Unmarshal([]byte(body), &stats); err != nil || stats.MaxOpen != 50 || stats.Open > 50 {
		t.Errorf("/stats: %s", body)
	}
	t.Logf("50 Cairo\\Sql queries running pg_sleep(1), on 1 PHP thread: %s (pool: %s)", took.Round(time.Millisecond), body)
}

func TestSqlMySQL(t *testing.T) {
	do := startSQLScript(t, "mysql.php", mysqlURL(t), "50")
	if code, body := do("POST", "/reset"); code != 200 {
		t.Fatalf("/reset: %d %s", code, body)
	}

	if code, body := do("POST", "/items?name=first"); code != 200 || body != `{"inserted":1}` {
		t.Errorf("insert: %d %s", code, body)
	}
	if code, body := do("GET", "/items"); code != 200 || body != `[{"id":1,"name":"first"}]` {
		t.Errorf("select: %d %s", code, body)
	}

	want := `{"int":42,"float":1.5,"text":"héllo","null":null,"bytes":"00ff10","time":"2026-01-02T03:04:05Z","date":"2026-01-02","decimal":"12.50"}`
	if code, body := do("GET", "/types"); code != 200 || body != want {
		t.Errorf("/types: %d\n got %s\nwant %s", code, body, want)
	}
	if code, body := do("GET", "/params"); code != 200 || body != `{"b":"00ff10","t":"2026-05-06T07:08:09Z","n":9007199254740994}` {
		t.Errorf("/params: %d %s", code, body)
	}

	if code, body := do("POST", "/tx?fail=1"); code != 200 || body != `{"count":1}` {
		t.Errorf("rolled-back transaction: %d %s, want the count unchanged", code, body)
	}
	if code, body := do("POST", "/tx"); code != 200 || body != `{"count":3}` {
		t.Errorf("committed transaction: %d %s, want 2 more rows", code, body)
	}

	for kind, state := range map[string]string{"table": "42S02", "unique": "23000"} {
		code, body := do("GET", "/error?kind="+kind)
		var e struct {
			SQLState string `json:"sqlstate"`
		}
		if err := json.Unmarshal([]byte(body), &e); code != 200 || err != nil || e.SQLState != state {
			t.Errorf("/error?kind=%s: %d %s, want SQLSTATE %s", kind, code, body, state)
		}
	}

	// 50 SLEEP(1) queries on one PHP thread wait in Go, together.
	start := time.Now()
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if code, body := do("GET", "/sleep"); code != 200 || body != "1" {
				t.Errorf("/sleep: %d %s", code, body)
			}
		}()
	}
	wg.Wait()
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("50 SLEEP(1) queries on 1 PHP thread took %s", took)
	}
}

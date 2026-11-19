package test

import (
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// examples/repeated logs each statement that one request ran 10 times or
// more, through PDO with every driver and through Cairo\Sql, with its route
// and shape, once a minute per route.
func TestRepeatedQueries(t *testing.T) {
	for _, db := range []struct {
		name, env, sql string
	}{
		{name: "sqlite"},
		{name: "mysql", env: "CAIRO_TEST_MYSQL"},
		{name: "pgsql", env: "CAIRO_TEST_PGSQL", sql: "pgsql"},
	} {
		t.Run(db.name, func(t *testing.T) {
			env := []string{}
			if db.env != "" {
				if os.Getenv(db.env) == "" {
					t.Skip(db.env + " is not set")
				}
				dsn := os.Getenv(db.env)
				if db.name == "mysql" {
					mysqlURL(t) // creates cairo_sql: /few needs a database for its temporary table
					dsn += ";dbname=cairo_sql"
				}
				env = append(env, "CAIRO_PDO_DSN="+dsn,
					"CAIRO_PDO_USER="+os.Getenv(db.env+"_USER"), "CAIRO_PDO_PASSWORD="+os.Getenv(db.env+"_PASSWORD"))
			}
			if db.sql != "" {
				env = append(env, "CAIRO_SQL_DSN="+sqlDSN(t))
			}
			checkRepeatedQueries(t, env, db.sql != "")
		})
	}
}

func checkRepeatedQueries(t *testing.T, env []string, withSQL bool) {
	t.Helper()
	addr := freeAddr()
	cmd := exec.Command(cairoBin, "serve", "../examples/repeated/server.php", "--threads", "1", "--offload-threads", "1")
	cmd.Env = append(append(os.Environ(), "CAIRO_HTTP="+addr), env...)
	var logs syncBuffer
	cmd.Stdout, cmd.Stderr = os.Stderr, &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		_ = cmd.Wait()
	})
	if err := waitFor(addr, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	paths := []string{"/orders", "/orders", "/inline", "/few"}
	want := 2
	if withSQL {
		paths = append(paths, "/sql")
		want++
	}
	for _, path := range paths {
		resp, err := http.Get("http://" + addr + path)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s: status %d\n%s", path, resp.StatusCode, logs.String())
		}
	}

	var lines []string
	for deadline := time.Now().Add(2 * time.Second); len(lines) < want && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		lines = lines[:0]
		for _, l := range strings.Split(logs.String(), "\n") {
			if strings.Contains(l, `msg="repeated query"`) {
				lines = append(lines, l)
			}
		}
	}
	all := strings.Join(lines, "\n")
	t.Log(all)
	// /orders ran twice but is logged once; /few is under the threshold.
	if len(lines) != want || strings.Contains(all, "/few") {
		t.Fatalf("want %d repeated query lines:\n%s", want, logs.String())
	}
	for _, w := range []string{
		`route="GET /orders" count=12 query="SELECT ? AS order_id"`,
		`route="GET /inline" count=10 query="SELECT ? AS n, ? AS s"`,
	} {
		if !strings.Contains(all, w) {
			t.Errorf("no %s in\n%s", w, all)
		}
	}
	if strings.Contains(logs.String(), "secret") {
		t.Errorf("a literal reached the log:\n%s", logs.String())
	}
	if withSQL && !strings.Contains(all, `route="GET /sql" count=10 query="SELECT $1::int AS id"`) {
		t.Errorf("no Cairo\\Sql line in\n%s", all)
	}
}

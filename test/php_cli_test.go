package test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const cliScript = "../examples/cli/command.php"

type cliRun struct {
	bin    string   // the program to start; empty is the cairo binary
	args   []string // its arguments
	stdin  string
	env    []string
	stdout string
	stderr string
	status int
}

// runCLI starts the binary and waits for it. Cairo's own files go to a
// directory of the test's, which has to be empty again afterwards.
func runCLI(t *testing.T, run cliRun) cliRun {
	t.Helper()
	bin := run.bin
	if bin == "" {
		bin = cairoBin
	}
	tmp := t.TempDir()
	cmd := exec.Command(bin, run.args...)
	cmd.Env = append(append(os.Environ(), "TMPDIR="+tmp), run.env...)
	cmd.Stdin = strings.NewReader(run.stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("%s %v: %v", bin, run.args, err)
	}
	run.stdout, run.stderr, run.status = stdout.String(), stderr.String(), cmd.ProcessState.ExitCode()
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("%v left %d entries in the temporary directory, the first is %s", run.args, len(left), left[0].Name())
	}
	return run
}

// The script gets what PHP's command line gives one: the SAPI is cli, $argv
// starts at the script, the script is the only included file, and PHP_BINARY
// is the cairo binary.
func TestPHPCommandLineRunsAScript(t *testing.T) {
	got := runCLI(t, cliRun{args: []string{"php", cliScript, "about", "one", "--two"}})
	if got.status != 0 || got.stderr != "" {
		t.Fatalf("status %d, stderr %q, stdout %q", got.status, got.stderr, got.stdout)
	}
	var about struct {
		SAPI, Binary, Script, Errors string
		Argv, Included               []string
		Cairo                        bool
	}
	if err := json.Unmarshal([]byte(got.stdout), &about); err != nil {
		t.Fatalf("%s: %v", got.stdout, err)
	}
	script, _ := filepath.Abs(cliScript)
	binary, _ := filepath.EvalSymlinks(cairoBin)
	if about.SAPI != "cli" || about.Binary != binary || about.Script != cliScript || about.Errors != "1" || !about.Cairo {
		t.Errorf("got %+v, want the cli SAPI, binary %s, errors shown and Cairo's classes", about, binary)
	}
	if strings.Join(about.Argv, " ") != cliScript+" about one --two" {
		t.Errorf("argv = %q", about.Argv)
	}
	if len(about.Included) != 1 || about.Included[0] != script {
		t.Errorf("included files = %q, want only %s", about.Included, script)
	}
}

// PHP's command line starts under three names: `cairo php`, a link to the
// binary that is called php, and the binary with PHP's own arguments, which
// is how PHP code starts PHP again (PHP_BINARY).
func TestPHPCommandLineNames(t *testing.T) {
	link := filepath.Join(t.TempDir(), "php")
	if err := os.Symlink(cairoBin, link); err != nil {
		t.Fatal(err)
	}
	code := `echo PHP_SAPI, " ", $argv[1];`
	cases := map[string]struct {
		run  cliRun
		want string
	}{
		"cairo php":           {cliRun{args: []string{"php", "-r", code, "a"}}, "cli a"},
		"a link named php":    {cliRun{bin: link, args: []string{"-r", code, "a"}}, "cli a"},
		"an option of PHP's":  {cliRun{args: []string{"-d", "memory_limit=64M", "-r", code, "a"}}, "cli a"},
		"a script":            {cliRun{args: []string{cliScript, "upper"}, stdin: "text"}, "TEXT"},
		"php starts php":      {cliRun{args: []string{"php", cliScript, "again"}}, `"about","again"],`},
		"the script on stdin": {cliRun{args: []string{"php", "--", "a"}, stdin: "<?php " + code}, "cli a"},
	}
	for name, c := range cases {
		got := runCLI(t, c.run)
		if got.status != 0 || !strings.Contains(got.stdout, c.want) {
			t.Errorf("%s: status %d, stdout %q, stderr %q; want %q", name, got.status, got.stdout, got.stderr, c.want)
		}
	}
	// A word that is neither a command of Cairo's nor a file stays an error.
	if got := runCLI(t, cliRun{args: []string{"serv"}}); got.status != 1 || !strings.Contains(got.stderr, `unknown command "serv"`) {
		t.Errorf("cairo serv: status %d, stderr %q", got.status, got.stderr)
	}
}

func TestPHPCommandLineOptions(t *testing.T) {
	cases := map[string]struct {
		run    cliRun
		status int
		stdout string // a regular expression
	}{
		"-r with arguments":          {cliRun{args: []string{"php", "-r", `echo $argc, $argv[0], $argv[2];`, "a", "-b"}}, 0, `^3Standard input code-b$`},
		"-d sets an INI entry":       {cliRun{args: []string{"php", "-d", "memory_limit=96M", "-dprecision=5", "-r", `echo ini_get("memory_limit"), " ", 1/3;`}}, 0, `^96M 0\.33333$`},
		"-d alone turns it on":       {cliRun{args: []string{"php", "-d", "short_open_tag", "-r", `echo ini_get("short_open_tag");`}}, 0, `^1$`},
		"-d quotes a value":          {cliRun{args: []string{"php", "-d", "error_prepend_string=<b>", "-r", `echo ini_get("error_prepend_string");`}}, 0, `^<b>$`},
		"-n and no time limit":       {cliRun{args: []string{"php", "-n", "-r", `echo ini_get("max_execution_time"), ini_get("html_errors"), var_export(php_ini_loaded_file(), true);`}}, 0, `^00false$`},
		"-f names the script":        {cliRun{args: []string{"php", "-f", cliScript, "upper"}, stdin: "text"}, 0, `^TEXT$`},
		"exit status":                {cliRun{args: []string{"php", cliScript, "fail", "7"}}, 7, `^$`},
		"uncaught exception":         {cliRun{args: []string{"php", "-r", `throw new RuntimeException("boom");`}}, 255, `Fatal error: Uncaught RuntimeException: boom in Command line code:1`},
		"a file that is missing":     {cliRun{args: []string{"php", "missing.php"}}, 1, `^$`},
		"-l, good and broken":        {cliRun{args: []string{"php", "-l", cliScript, "../examples/cli/broken.php", cliScript}}, 255, `(?s)^No syntax errors detected in \S+command\.php\n.*Parse error.*Errors parsing \S+broken\.php\nNo syntax errors detected`},
		"-v":                         {cliRun{args: []string{"php", "-v"}}, 0, `^PHP \d+\.\d+\.\d+\S* \(cli\) \(ZTS\)\n(?s).*with Cairo`},
		"-m":                         {cliRun{args: []string{"php", "-m"}}, 0, `(?s)^\[PHP Modules\]\n.*\nCore\n.*\[Zend Modules\]`},
		"-i":                         {cliRun{args: []string{"php", "-i"}}, 0, `Server API => Command Line Interface`},
		"--ini":                      {cliRun{args: []string{"php", "--ini"}}, 0, `Loaded Configuration File:`},
		"--ri":                       {cliRun{args: []string{"php", "--ri", "json"}}, 0, `json support => enabled`},
		"-h":                         {cliRun{args: []string{"php", "-h"}}, 0, `^usage:\n  cairo php `},
		"an option it lacks":         {cliRun{args: []string{"php", "-a"}}, 1, `^$`},
		"a shebang line is code":     {cliRun{args: []string{"php", cliScript, "nothing"}}, 2, `^$`},
		"STDIN is not the code":      {cliRun{args: []string{"php", "-r", `echo strrev(fgets(STDIN));`}, stdin: "abc"}, 0, `^cba$`},
		"Cairo's tasks run in place": {cliRun{args: []string{"php", "-r", `use function Cairo\{async, await, all, offload}; echo json_encode(await(all([async(fn () => 1), async(function () { Cairo\sleep(0.001); return offload("strtoupper", "x"); })]))); try { await(async(fn () => throw new DomainException("no"))); } catch (DomainException $e) { echo $e->getMessage(); }`}}, 0, `^\[1,"X"\]no$`},
		"a fiber of the script":      {cliRun{args: []string{"php", "-r", `$f = new Fiber(function () { usleep(1000); echo gethostbyname("localhost"), Fiber::suspend(1); }); echo $f->start(); $f->resume("!");`}}, 0, `^127\.0\.0\.11!$`},
	}
	for name, c := range cases {
		got := runCLI(t, c.run)
		if got.status != c.status || !regexp.MustCompile(c.stdout).MatchString(got.stdout) {
			t.Errorf("%s: status %d, stdout %q, stderr %q; want status %d and stdout like %s", name, got.status, got.stdout, got.stderr, c.status, c.stdout)
		}
	}
}

// What the script writes to each stream goes to that stream, and so do the
// errors PHP reports: shown on standard output, as with PHP.
func TestPHPCommandLineStreams(t *testing.T) {
	got := runCLI(t, cliRun{args: []string{"php", cliScript, "fail", "3"}})
	if got.stdout != "" || got.stderr != "the command failed\n" || got.status != 3 {
		t.Errorf("status %d, stdout %q, stderr %q", got.status, got.stdout, got.stderr)
	}
	got = runCLI(t, cliRun{args: []string{"php", "missing.php"}})
	if got.stderr != "Could not open input file: missing.php\n" {
		t.Errorf("a missing script: stderr %q", got.stderr)
	}
	got = runCLI(t, cliRun{args: []string{"php", "-r", `echo $undefined; echo "after";`}})
	if !strings.Contains(got.stdout, "Warning: Undefined variable $undefined") || !strings.HasSuffix(got.stdout, "after") || got.stderr != "" {
		t.Errorf("a warning: stdout %q, stderr %q", got.stdout, got.stderr)
	}
}

// A migration through PDO, with Cairo's own PostgreSQL driver and with
// PDO MySQL: outside a server a query waits on the thread.
func TestPHPCommandLineDatabases(t *testing.T) {
	for _, driver := range []string{"pgsql", "mysql"} {
		t.Run(driver, func(t *testing.T) {
			if os.Getenv("CAIRO_TEST_"+strings.ToUpper(driver)) == "" {
				t.Skipf("CAIRO_TEST_%s is not set", strings.ToUpper(driver))
			}
			got := runCLI(t, cliRun{args: []string{"php", cliScript, "migrate", driver}})
			if got.status != 0 || got.stdout != "migrated 2 releases with "+driver+"\n" {
				t.Errorf("status %d, stdout %q, stderr %q", got.status, got.stdout, got.stderr)
			}
		})
	}
}

// php.ini is the one -c names, or CAIRO_PHP_INI as for the server; -d wins
// over it and -n reads none.
func TestPHPCommandLineReadsPHPIni(t *testing.T) {
	ini := filepath.Join(t.TempDir(), "php.ini")
	if err := os.WriteFile(ini, []byte("memory_limit=77M\ndisplay_errors=0\noutput_buffering=4096\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code := `echo ini_get("memory_limit"), " ", ini_get("display_errors"), " ", ini_get("output_buffering"), " ", php_ini_loaded_file() ?: "none";`
	cases := map[string]struct {
		run  cliRun
		want string
	}{
		"-c":                    {cliRun{args: []string{"php", "-c", ini, "-r", code}}, "77M 0 0 " + ini},
		"CAIRO_PHP_INI":         {cliRun{args: []string{"php", "-r", code}, env: []string{"CAIRO_PHP_INI=" + ini}}, "77M 0 0 " + ini},
		"-d over php.ini":       {cliRun{args: []string{"php", "-c", ini, "-d", "memory_limit=99M", "-r", code}}, "99M 0 0 " + ini},
		"-n over CAIRO_PHP_INI": {cliRun{args: []string{"php", "-n", "-r", code}, env: []string{"CAIRO_PHP_INI=" + ini}}, "128M 1 0 none"},
	}
	for name, c := range cases {
		if got := runCLI(t, c.run); got.status != 0 || got.stdout != c.want {
			t.Errorf("%s: status %d, stdout %q, stderr %q; want %q", name, got.status, got.stdout, got.stderr, c.want)
		}
	}
}

package test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const signalsScript = "../examples/cli/signals.php"

// signalled is a command of `cairo php` that the test sends signals.
type signalled struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdout *bufio.Reader
	stderr bytes.Buffer
	done   chan error
}

// startSignalled starts cairo php with args and returns once the script
// said it is ready: its handlers are in place. The scheduler is on unless
// the environment says otherwise.
func startSignalled(t *testing.T, env []string, args ...string) *signalled {
	t.Helper()
	s := &signalled{t: t, cmd: exec.Command(cairoBin, append([]string{"php"}, args...)...), done: make(chan error, 1)}
	s.cmd.Env = append(os.Environ(), env...)
	out, err := s.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	s.stdout = bufio.NewReader(out)
	s.cmd.Stderr = &s.stderr
	if err := s.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.cmd.Process.Kill() })
	// A script that hangs must not hang the tests.
	timer := time.AfterFunc(20*time.Second, func() { s.cmd.Process.Kill() })
	t.Cleanup(func() { timer.Stop() })
	if line := s.line(); line != "ready" {
		t.Fatalf("%v: first line %q, want ready; stderr %q", args, line, s.stderr.String())
	}
	return s
}

// line is the script's next line of output, "" once it has ended.
func (s *signalled) line() string {
	line, _ := s.stdout.ReadString('\n')
	return strings.TrimSuffix(line, "\n")
}

func (s *signalled) signal(sig syscall.Signal) {
	s.t.Helper()
	if err := s.cmd.Process.Signal(sig); err != nil {
		s.t.Fatal(err)
	}
}

// end reads what the script still prints and returns it with the state the
// process ended in.
func (s *signalled) end() (string, *os.ProcessState) {
	var rest strings.Builder
	for line, err := s.stdout.ReadString('\n'); line != "" || err == nil; line, err = s.stdout.ReadString('\n') {
		rest.WriteString(line)
	}
	s.cmd.Wait()
	return rest.String(), s.cmd.ProcessState
}

// The two ways a script runs: as a fiber of Cairo's scheduler, and as PHP
// runs it.
var signalModes = map[string][]string{
	"scheduler":    nil,
	"no scheduler": {"CAIRO_SCHEDULER=off"},
}

// A worker's loop, as Laravel's queue worker has it: SIGUSR1 reports and
// SIGTERM stops it, and each ends the sleep() the worker is in, as under PHP.
func TestPHPSignalsStopAWorker(t *testing.T) {
	for mode, env := range signalModes {
		t.Run(mode, func(t *testing.T) {
			start := time.Now()
			worker := startSignalled(t, env, signalsScript, "worker")
			worker.signal(syscall.SIGUSR1)
			if a, b := worker.line(), worker.line(); a != "jobs so far: 1" || !strings.HasPrefix(b, "woken with ") {
				t.Fatalf("after SIGUSR1: %q, %q; stderr %q", a, b, worker.stderr.String())
			}
			worker.signal(syscall.SIGTERM)
			rest, state := worker.end()
			if !strings.HasSuffix(rest, "stopped after the job in hand\n") || state.ExitCode() != 0 {
				t.Errorf("after SIGTERM: %q, status %d, stderr %q", rest, state.ExitCode(), worker.stderr.String())
			}
			if took := time.Since(start); took > 10*time.Second {
				t.Errorf("took %v: the signals did not end the sleep of 30 s", took)
			}
		})
	}
}

// Code that never waits is interrupted where it runs: under the scheduler
// that is inside its fiber.
func TestPHPSignalsInterruptBusyCode(t *testing.T) {
	want := map[string]string{"scheduler": "handled in a fiber\n", "no scheduler": "handled outside a fiber\n"}
	for mode, env := range signalModes {
		t.Run(mode, func(t *testing.T) {
			busy := startSignalled(t, env, signalsScript, "busy")
			busy.signal(syscall.SIGUSR1)
			if rest, state := busy.end(); rest != want[mode] || state.ExitCode() != 0 {
				t.Errorf("got %q, status %d, stderr %q", rest, state.ExitCode(), busy.stderr.String())
			}
		})
	}
}

// Fifty fibers that each wait a second take a second together with a handler
// in place, and the handler runs while all of them wait, in a fiber: the
// fiber that waits for signals keeps nobody.
func TestPHPSignalsWhileFibersWait(t *testing.T) {
	tasks := startSignalled(t, nil, signalsScript, "tasks", "50")
	tasks.signal(syscall.SIGUSR1)
	rest, state := tasks.end()
	if rest != "50 waits of 1 s, 50 saw the signal handled in a fiber\n" || state.ExitCode() != 0 {
		t.Errorf("got %q, status %d, stderr %q", rest, state.ExitCode(), tasks.stderr.String())
	}
}

// pcntl_alarm() gives a job a time limit: the handler ends the script while
// its fibers wait.
func TestPHPSignalsAlarm(t *testing.T) {
	for mode, env := range signalModes {
		t.Run(mode, func(t *testing.T) {
			rest, state := startSignalled(t, env, signalsScript, "limit").end()
			if rest != "the job took too long\n" || state.ExitCode() != 3 {
				t.Errorf("got %q, status %d", rest, state.ExitCode())
			}
		})
	}
}

// A script waits for a signal as its fibers wait for anything, on a channel
// that the handler sends to: it is not a script that nothing can wake.
func TestPHPSignalsWakeAScriptThatWaits(t *testing.T) {
	wait := startSignalled(t, nil, signalsScript, "wait")
	wait.signal(syscall.SIGINT)
	if rest, state := wait.end(); rest != "signal 2\n" || state.ExitCode() != 0 {
		t.Errorf("got %q, status %d, stderr %q", rest, state.ExitCode(), wait.stderr.String())
	}
}

// PHP lets no fiber switch inside a handler: with several fibers a handler
// that waits is refused, there and then, and the script's waits go on. A
// script with one fiber waits with its thread, in a handler too.
func TestPHPSignalsHandlerThatWaits(t *testing.T) {
	const handler = `pcntl_async_signals(true);
		pcntl_signal(SIGUSR1, function () {
			try { echo gethostbyname("localhost"), "\n"; } catch (FiberError) { echo "refused\n"; }
		});`
	for name, c := range map[string]struct{ code, want string }{
		"several fibers": {`$task = Cairo\async(function () { Cairo\sleep(0.2); return gethostbyname("localhost"); });
			posix_kill(getmypid(), SIGUSR1); echo Cairo\await($task), "\n";`, "refused\n127.0.0.1\n"},
		"one fiber": {`posix_kill(getmypid(), SIGUSR1); sleep(1); echo gethostbyname("localhost"), "\n";`, "127.0.0.1\n127.0.0.1\n"},
	} {
		got := runCLI(t, cliRun{args: []string{"php", "-r", handler + c.code}})
		if got.stdout != c.want || got.status != 0 {
			t.Errorf("%s: %q, status %d, stderr %q", name, got.stdout, got.status, got.stderr)
		}
	}
}

// A signal whose handler the script took off again does what it does
// without one, and so does a signal that never had one.
func TestPHPSignalsDefaultAction(t *testing.T) {
	codes := map[string]string{
		"handler taken off": `pcntl_signal(SIGTERM, fn () => null); pcntl_signal(SIGTERM, SIG_DFL); echo "ready\n"; sleep(30);`,
		"another signal's":  `pcntl_signal(SIGUSR1, fn () => null); echo "ready\n"; sleep(30);`,
	}
	for name, code := range codes {
		for mode, env := range signalModes {
			script := startSignalled(t, env, "-r", code)
			script.signal(syscall.SIGTERM)
			_, state := script.end()
			if status, ok := state.Sys().(syscall.WaitStatus); !ok || !status.Signaled() || status.Signal() != syscall.SIGTERM {
				t.Errorf("%s, %s: ended with %v, want killed by SIGTERM", name, mode, state)
			}
		}
	}
	// An ignored signal does not even end the sleep.
	ignored := startSignalled(t, nil, "-r", `pcntl_signal(SIGTERM, SIG_IGN); echo "ready\n"; echo "left ", sleep(1), "\n";`)
	ignored.signal(syscall.SIGTERM)
	if rest, state := ignored.end(); rest != "left 0\n" || state.ExitCode() != 0 {
		t.Errorf("an ignored SIGTERM: %q, ended with %v", rest, state)
	}
}

// What a script has of pcntl: its signal functions and the rest, but nothing
// that forks or waits for a signal with the thread.
func TestPHPSignalsFunctions(t *testing.T) {
	got := runCLI(t, cliRun{args: []string{"php", signalsScript, "about"}})
	var about struct {
		Functions   map[string]bool
		Kept, Reset bool
	}
	if err := json.Unmarshal([]byte(got.stdout), &about); err != nil {
		t.Fatalf("%q, stderr %q: %v", got.stdout, got.stderr, err)
	}
	for function, has := range about.Functions {
		if want := function != "pcntl_fork" && function != "pcntl_sigwaitinfo"; has != want {
			t.Errorf("%s exists: %v, want %v", function, has, want)
		}
	}
	if !about.Kept || !about.Reset {
		t.Errorf("pcntl_signal_get_handler(): %+v", about)
	}
}

// CAIRO_PHP_CLI hands a command to another PHP: the process becomes it, with
// the arguments PHP's command line was given. Cairo itself is refused.
func TestPHPCommandLineHandsToAnotherPHP(t *testing.T) {
	other := filepath.Join(t.TempDir(), "other-php")
	if err := os.WriteFile(other, []byte("#!/bin/sh\necho \"$0 got: $*\"\nexit 7\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "php")
	if err := os.Symlink(cairoBin, link); err != nil {
		t.Fatal(err)
	}
	args := []string{"-d", "memory_limit=1G", "artisan", "queue:work"}
	for name, run := range map[string]cliRun{
		"cairo php":        {args: append([]string{"php"}, args...)},
		"a link named php": {bin: link, args: args},
		"PHP's arguments":  {args: args},
	} {
		run.env = []string{"CAIRO_PHP_CLI=" + other}
		got := runCLI(t, run)
		if got.status != 7 || got.stdout != other+" got: -d memory_limit=1G artisan queue:work\n" {
			t.Errorf("%s: status %d, stdout %q, stderr %q", name, got.status, got.stdout, got.stderr)
		}
	}
	for name, php := range map[string]string{"cairo itself": link, "no such program": filepath.Join(t.TempDir(), "none")} {
		got := runCLI(t, cliRun{args: []string{"php", "-v"}, env: []string{"CAIRO_PHP_CLI=" + php}})
		if got.status != 1 || !strings.Contains(got.stderr, "CAIRO_PHP_CLI="+php) || got.stdout != "" {
			t.Errorf("%s: status %d, stdout %q, stderr %q", name, got.status, got.stdout, got.stderr)
		}
	}
	// `cairo serve` is not PHP's command line and stays Cairo's.
	if got := runCLI(t, cliRun{args: []string{"version"}, env: []string{"CAIRO_PHP_CLI=" + other}}); got.status != 0 || strings.Contains(got.stdout, "got:") {
		t.Errorf("cairo version: status %d, stdout %q", got.status, got.stdout)
	}
}

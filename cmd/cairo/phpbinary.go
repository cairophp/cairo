package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// otherPHPEnv names a PHP command line that runs commands in place of the
// one in this binary: an ordinary PHP has all of pcntl, fork included, and
// what else this binary leaves out. It is none of Cairo: no Cairo\ classes,
// no scheduler, and PostgreSQL through its own driver.
const otherPHPEnv = "CAIRO_PHP_CLI"

// handToOtherPHP turns the process into the PHP that CAIRO_PHP_CLI names,
// with the arguments PHP's command line was given: its signals, its exit
// status and its PHP_BINARY are then that PHP's own. It returns only when
// nothing is named (handed is false) or the PHP could not be started.
func handToOtherPHP(args []string) (status int, handed bool) {
	name := os.Getenv(otherPHPEnv)
	if name == "" {
		return 0, false
	}
	path, err := otherPHP(name)
	if err == nil {
		err = syscall.Exec(path, append([]string{path}, args...), os.Environ())
	}
	fmt.Fprintf(os.Stderr, "cairo php: %s=%s: %v\n", otherPHPEnv, name, err)
	return 1, true
}

// otherPHP finds the program name, by PATH when it is no path, and refuses
// this binary: under any name it would hand the command to itself for ever.
func otherPHP(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	other, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if cairo, err := os.Stat(self); err == nil && os.SameFile(other, cairo) {
		return "", fmt.Errorf("that is cairo itself: name a PHP command line")
	}
	return path, nil
}

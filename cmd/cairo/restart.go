package main

import (
	"os"
	"syscall"
)

// restartProcess replaces this process with a new one of the same command,
// after the servers and PHP have stopped. It keeps the process id, so
// whatever started the server (a shell, systemd, a container) still has it.
// Until the new process listens, connections are refused.
func restartProcess() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return syscall.Exec(exe, os.Args, os.Environ())
}

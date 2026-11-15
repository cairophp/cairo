package sapi

import (
	"context"
	"errors"
	"net"
	"syscall"
	"time"
)

// dialFD connects with Go's dialer (Go's resolver, netpoller and ctx) and
// hands PHP a blocking duplicate of the socket. Go's own descriptor is closed;
// the connection stays open through the duplicate, which PHP then owns.
func dialFD(ctx context.Context, network, address string, timeout time.Duration) (int, error) {
	d := net.Dialer{}
	if timeout > 0 {
		d.Timeout = timeout
	}
	c, err := d.DialContext(ctx, network, address)
	if err != nil {
		return -1, err
	}
	defer c.Close()
	sc, ok := c.(syscall.Conn)
	if !ok {
		return -1, errors.New("connection has no file descriptor")
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return -1, err
	}
	fd := -1
	var dupErr error
	if err := raw.Control(func(s uintptr) {
		syscall.ForkLock.RLock()
		fd, dupErr = syscall.Dup(int(s))
		if dupErr == nil {
			syscall.CloseOnExec(fd)
		}
		syscall.ForkLock.RUnlock()
	}); err != nil {
		return -1, err
	}
	if dupErr != nil {
		return -1, dupErr
	}
	// PHP's socket streams expect a blocking descriptor.
	if err := syscall.SetNonblock(fd, false); err != nil {
		syscall.Close(fd)
		return -1, err
	}
	return fd, nil
}

// lookupIPv4 resolves host to its IPv4 addresses, as gethostbyname() does.
func lookupIPv4(ctx context.Context, host string) ([]string, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, a := range addrs {
		if v4 := a.IP.To4(); v4 != nil {
			out = append(out, v4.String())
		}
	}
	return out, nil
}

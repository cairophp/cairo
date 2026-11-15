package sapi

import (
	"context"
	"errors"
	"sync"
	"syscall"
	"time"
)

// poller waits for PHP's socket descriptors to become readable or writable
// without holding an OS thread per waiter: one epoll instance, one goroutine
// reading its events. PHP keeps owning the descriptors (they stay blocking);
// Cairo only registers interest while a fiber waits on one.
type poller struct {
	epfd    int
	mu      sync.Mutex
	waiters map[int32]chan struct{}
}

var (
	pollerOnce sync.Once
	pollerInst *poller
	pollerErr  error
)

func getPoller() (*poller, error) {
	pollerOnce.Do(func() {
		fd, err := syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
		if err != nil {
			pollerErr = err
			return
		}
		pollerInst = &poller{epfd: fd, waiters: map[int32]chan struct{}{}}
		go pollerInst.loop()
	})
	return pollerInst, pollerErr
}

func (p *poller) loop() {
	events := make([]syscall.EpollEvent, 128)
	for {
		n, err := syscall.EpollWait(p.epfd, events, -1)
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			return
		}
		p.mu.Lock()
		for _, ev := range events[:n] {
			if ch, ok := p.waiters[ev.Fd]; ok {
				select {
				case ch <- struct{}{}:
				default:
				}
			}
		}
		p.mu.Unlock()
	}
}

// errPollTimeout means the socket's own timeout passed (not the context's).
var errPollTimeout = errors.New("socket timeout")

// wait blocks until fd is ready for reading (or writing), the timeout passes
// (timeout < 0 means none), or ctx ends. One fiber waits on a descriptor at a
// time: PHP connections are not shared between fibers.
func (p *poller) wait(ctx context.Context, fd int, write bool, timeout time.Duration) error {
	if write {
		return p.waitSet(ctx, nil, []int{fd}, timeout)
	}
	return p.waitSet(ctx, []int{fd}, nil, timeout)
}

// waitSet blocks until any descriptor in read is readable or any in write is
// writable, the timeout passes, or ctx ends. An empty set just waits out the
// timeout.
func (p *poller) waitSet(ctx context.Context, read, write []int, timeout time.Duration) error {
	want := map[int]uint32{}
	for _, fd := range read {
		want[fd] |= syscall.EPOLLIN | syscall.EPOLLRDHUP
	}
	for _, fd := range write {
		want[fd] |= syscall.EPOLLOUT
	}
	ch := make(chan struct{}, 1)

	p.mu.Lock()
	for fd := range want {
		if _, busy := p.waiters[int32(fd)]; busy {
			p.mu.Unlock()
			return errors.New("socket is already in use by another fiber")
		}
	}
	for fd := range want {
		p.waiters[int32(fd)] = ch
	}
	p.mu.Unlock()
	var added []int
	defer func() {
		p.mu.Lock()
		for fd := range want {
			delete(p.waiters, int32(fd))
		}
		p.mu.Unlock()
		for _, fd := range added {
			_ = syscall.EpollCtl(p.epfd, syscall.EPOLL_CTL_DEL, fd, nil)
		}
	}()

	for fd, events := range want {
		ev := syscall.EpollEvent{Events: events | syscall.EPOLLONESHOT, Fd: int32(fd)}
		err := syscall.EpollCtl(p.epfd, syscall.EPOLL_CTL_ADD, fd, &ev)
		if errors.Is(err, syscall.EEXIST) {
			err = syscall.EpollCtl(p.epfd, syscall.EPOLL_CTL_MOD, fd, &ev)
		}
		if err != nil {
			return err
		}
		added = append(added, fd)
	}

	var timer <-chan time.Time
	if timeout >= 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timer = t.C
	}
	select {
	case <-ch:
		return nil
	case <-timer:
		return errPollTimeout
	case <-ctx.Done():
		return ctx.Err()
	}
}

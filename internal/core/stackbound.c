// The stack bounds of a thread, read from libc once.
//
// On a call from C into Go, Go's runtime checks that the stack pointer is
// inside the stack it knows for the thread. A PHP fiber runs on its own
// stack, so every call from a fiber fails that check, and so does the next
// one from the thread's own stack, and each time the runtime asks libc for
// the thread's stack with pthread_getattr_np(). glibc answers with all of
// the thread's attributes, its CPU affinity among them: an allocation and a
// sched_getaffinity system call, for a stack that never moves.
//
// The linker sends the calls of this binary to the function below
// (-Wl,--wrap in stackbound.go). It remembers the stack and its guard, which
// is all that the callers linked into the binary read (Go's runtime, and PHP
// in a static build); it does not remember the scheduling attributes, the
// affinity or the signal mask, which can change. musl has no affinity to
// fetch and Go's runtime does not ask there, so other libcs pass through.

#include <pthread.h>
#include <sys/syscall.h>
#include <unistd.h>

#include "stackbound.h"

int __real_pthread_getattr_np(pthread_t thread, pthread_attr_t *attr);

#ifdef __GLIBC__

enum { BOUND_UNKNOWN, BOUND_KNOWN, BOUND_GROWS };

static __thread struct {
	int state;
	int lookups;
	void *addr;
	size_t size;
	size_t guard;
} bound;

static int bound_lookup(pthread_attr_t *attr)
{
	bound.lookups++;
	int err = __real_pthread_getattr_np(pthread_self(), attr);
	if (err != 0 || bound.state == BOUND_GROWS) {
		return err;
	}
	// The stack of the process's first thread grows, and glibc reads it
	// from /proc on every call: never remembered.
	if (syscall(SYS_gettid) == getpid()) {
		bound.state = BOUND_GROWS;
		return 0;
	}
	if (pthread_attr_getstack(attr, &bound.addr, &bound.size) == 0
			&& pthread_attr_getguardsize(attr, &bound.guard) == 0) {
		bound.state = BOUND_KNOWN;
	}
	return 0;
}

int __wrap_pthread_getattr_np(pthread_t thread, pthread_attr_t *attr)
{
	if (!pthread_equal(thread, pthread_self())) {
		return __real_pthread_getattr_np(thread, attr);
	}
	if (bound.state != BOUND_KNOWN) {
		return bound_lookup(attr);
	}
	int err = pthread_attr_init(attr);
	if (err != 0) {
		return err;
	}
	err = pthread_attr_setguardsize(attr, bound.guard);
	if (err == 0) {
		err = pthread_attr_setstack(attr, bound.addr, bound.size);
	}
	if (err != 0) {
		pthread_attr_destroy(attr);
	}
	return err;
}

static int bound_lookups(void)
{
	return bound.lookups;
}

#else

int __wrap_pthread_getattr_np(pthread_t thread, pthread_attr_t *attr)
{
	return __real_pthread_getattr_np(thread, attr);
}

static int bound_lookups(void)
{
	return 0;
}

#endif

int cairo_stackbound_probe(uintptr_t bounds[2])
{
	pthread_attr_t attr;
	void *addr;
	size_t size;

	if (pthread_getattr_np(pthread_self(), &attr) != 0) {
		return -1;
	}
	int err = pthread_attr_getstack(&attr, &addr, &size);
	pthread_attr_destroy(&attr);
	if (err != 0) {
		return -1;
	}
	bounds[0] = (uintptr_t)addr;
	bounds[1] = (uintptr_t)addr + size;
	return bound_lookups();
}

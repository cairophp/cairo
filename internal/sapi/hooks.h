#ifndef CAIRO_HOOKS_H
#define CAIRO_HOOKS_H

#include <stdbool.h>
#include <stdint.h>
#include <time.h>

#include "extension.h"

// The clock of cairo_stack_deadline() and PHP's hrtime(), in nanoseconds.
static inline int64_t cairo_mono_ns(void)
{
	struct timespec ts;
	clock_gettime(CLOCK_MONOTONIC, &ts);
	return (int64_t)ts.tv_sec * 1000000000 + ts.tv_nsec;
}

// Waits for socket fd to be readable (or writable), for a hook of a call
// that would block: timeout seconds at most (below 0: none). Inside a fiber
// only the fiber waits (Cairo\Internal\Hooks::wait); outside one, a
// classic request's wait ends at its deadline. Returns 1 when the socket is
// ready, 0 when the runtime declined (block as usual), -1 at the timeout,
// -2 with an exception thrown (the context ended, the deadline). pgsql: the
// wait is libpq's, under Hook::PGSQL instead of Hook::SOCKETS.
int cairo_hook_wait(int fd, int write, double timeout, bool pgsql);

// The runtime hooks; hookops.go adds them to the SAPI module.
extern const cairo_extension cairo_function_hooks;   // function_hook.c
extern const cairo_extension cairo_socket_hooks;     // socket_hook.c

#endif

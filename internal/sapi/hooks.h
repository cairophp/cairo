#ifndef CAIRO_HOOKS_H
#define CAIRO_HOOKS_H

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

// The runtime hooks; hookops.go adds them to the SAPI module.
extern const cairo_extension cairo_function_hooks;   // function_hook.c
extern const cairo_extension cairo_socket_hooks;     // socket_hook.c

#endif

#ifndef CAIRO_PARKED_H
#define CAIRO_PARKED_H

#include <stdint.h>

#include <php.h>

// The fibers that wait on their PHP thread (parked.c): in a sleep, in
// select(), on a socket, for an offload job. A parked fiber runs again when
// its time is up, when cairo_wake() names its wait, or when PHP ends the
// context it waits under (Waits in waits.php, which decides all that; this
// keeps the fibers and their timers). The PHP functions are included once,
// by cairo.c, for its function table.

// Wakes the fibers of the calling thread whose time is up, and returns how
// long until the next one's is, in nanoseconds; below 0 with no timer set.
int64_t cairo_parked_tick(void);

// What cairo_park() does, for a wait that begins in C: parks the running
// fiber as wait id under context of request, until cairo_parked_wake(id) or
// the monotonic clock is at at. kind is what it waits in (a constant of
// Waits). Returns once the fiber runs again; with an exception thrown when
// there is no fiber to park.
void cairo_park_fiber(zend_long id, zend_object *context, zend_long request, int64_t at, int kind);

// Ends wait id of the calling thread if it still goes on: its fiber runs at
// the loop's next turn.
void cairo_parked_wake(zend_long id);

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_park, 0, 0, 5)
	ZEND_ARG_INFO(0, id)
	ZEND_ARG_INFO(0, context)
	ZEND_ARG_INFO(0, request)
	ZEND_ARG_INFO(0, at)
	ZEND_ARG_INFO(0, kind)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_wake, 0, 0, 1)
	ZEND_ARG_INFO(0, id)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_parked, 0, 0, 0)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_parked_under, 0, 0, 1)
	ZEND_ARG_INFO(0, request)
ZEND_END_ARG_INFO()

PHP_FUNCTION(cairo_park);
PHP_FUNCTION(cairo_wake);
PHP_FUNCTION(cairo_parked);
PHP_FUNCTION(cairo_parked_under);

#endif

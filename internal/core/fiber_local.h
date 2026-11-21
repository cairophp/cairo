#ifndef CAIRO_FIBER_LOCAL_H
#define CAIRO_FIBER_LOCAL_H

#include <php.h>
#include <zend_fibers.h>

// Request-local statics (Cairo\RequestLocal): static properties that hold
// one request's state. Each request on a thread keeps its own value, swapped
// into the property when one of its fibers runs.

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_local_register, 0, 0, 2)
	ZEND_ARG_INFO(0, class)
	ZEND_ARG_INFO(0, property)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_local_end, 0, 0, 1)
	ZEND_ARG_INFO(0, request)
ZEND_END_ARG_INFO()

PHP_FUNCTION(cairo_local_register);
PHP_FUNCTION(cairo_local_end);

// Fiber f runs for request from now on (0: for none), with that request's
// values in the statics. Called from PHP functions only (fibers.c), before
// the fiber runs for the request.
void cairo_local_bind_fiber(zend_fiber *f, zend_long request);

// Registers the fiber switch observer, at module startup.
void cairo_fiber_local_startup(void);

#endif

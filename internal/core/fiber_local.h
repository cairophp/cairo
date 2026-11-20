#ifndef CAIRO_FIBER_LOCAL_H
#define CAIRO_FIBER_LOCAL_H

#include <php.h>

// Request-local statics (Cairo\RequestLocal): static properties that hold
// one request's state. Each request on a thread keeps its own value, swapped
// into the property when one of its fibers runs. Included once, by cairo.c,
// for its function table.

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_local_register, 0, 0, 2)
	ZEND_ARG_INFO(0, class)
	ZEND_ARG_INFO(0, property)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_local_bind, 0, 0, 2)
	ZEND_ARG_INFO(0, fiber)
	ZEND_ARG_INFO(0, request)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_local_end, 0, 0, 1)
	ZEND_ARG_INFO(0, request)
ZEND_END_ARG_INFO()

PHP_FUNCTION(cairo_local_register);
PHP_FUNCTION(cairo_local_bind);
PHP_FUNCTION(cairo_local_end);

// Registers the fiber switch observer, at module startup.
void cairo_fiber_local_startup(void);

#endif

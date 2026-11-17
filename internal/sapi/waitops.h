#ifndef CAIRO_WAITOPS_H
#define CAIRO_WAITOPS_H

#include <php.h>

// cairo_fiber_wait(), cairo_fiber_waitset() and cairo_fiber_call(): the
// most frequent fiber operations, sent to Go without JSON. Included once, by cairo.c, for its function table.

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_fiber_wait, 0, 0, 4)
	ZEND_ARG_INFO(0, fiber)
	ZEND_ARG_INFO(0, fd)
	ZEND_ARG_INFO(0, write)
	ZEND_ARG_INFO(0, timeout)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_fiber_waitset, 0, 0, 4)
	ZEND_ARG_INFO(0, fiber)
	ZEND_ARG_INFO(0, read)
	ZEND_ARG_INFO(0, write)
	ZEND_ARG_INFO(0, timeout)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_fiber_call, 0, 0, 3)
	ZEND_ARG_INFO(0, fiber)
	ZEND_ARG_INFO(0, op)
	ZEND_ARG_INFO(0, payload)
ZEND_END_ARG_INFO()

PHP_FUNCTION(cairo_fiber_wait);
PHP_FUNCTION(cairo_fiber_call);
PHP_FUNCTION(cairo_fiber_waitset);

#endif

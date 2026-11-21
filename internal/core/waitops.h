#ifndef CAIRO_WAITOPS_H
#define CAIRO_WAITOPS_H

#include <php.h>

// cairo_fiber_call(): what a fiber asks Go for; and cairo_op_cancel(),
// which ends such an operation early. Included once, by cairo.c, for its
// function table.

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_fiber_call, 0, 0, 4)
	ZEND_ARG_INFO(0, request)
	ZEND_ARG_INFO(0, scope)
	ZEND_ARG_INFO(0, op)
	ZEND_ARG_INFO(0, payload)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_op_cancel, 0, 0, 1)
	ZEND_ARG_INFO(0, id)
ZEND_END_ARG_INFO()

PHP_FUNCTION(cairo_fiber_call);
PHP_FUNCTION(cairo_op_cancel);

#endif

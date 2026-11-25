#ifndef CAIRO_FIBERS_H
#define CAIRO_FIBERS_H

#include <php.h>
#include <zend_fibers.h>

// The fibers of a PHP thread, as its scheduler keeps them (fibers.c): the
// context each one runs under, the idle ones that wait for their next job,
// and the queue of those to resume. scheduler.php decides what runs; this
// is where it switches. The PHP functions are included once, by cairo.c,
// for its function table.

// Resumes fiber, which must be suspended, with value (NULL: null), as
// Fiber::resume() does. *ret is what the fiber suspends with or null; an
// exception that the fiber throws is the caller's (EG(exception)).
void cairo_fiber_resume(zend_fiber *fiber, zval *value, zval *ret);

// Whether the code that runs may switch fibers; throws FiberError when it
// may not, as in a signal handler of pcntl's.
bool cairo_fiber_may_switch(void);

// Suspends the running fiber, as Fiber::suspend() does, and returns when
// it is resumed, with *ret what it is resumed with. FAILURE with an
// exception: there is no fiber to suspend, or it is being destroyed.
zend_result cairo_fiber_suspend(zval *ret);

// Queues fiber, which is suspended, for cairo_run_ready() to resume; and
// how many fibers are queued.
void cairo_fibers_queue(zend_fiber *fiber);
uint32_t cairo_fibers_queued(void);

// At module startup.
zend_result cairo_fibers_startup(void);

// At request shutdown, after the destructors: frees the stacks of the
// fibers that a fatal error left suspended.
void cairo_fibers_shutdown(void);

// After request shutdown: the lists of the thread's fibers were the
// request's memory, and are empty for the thread's next PHP request.
void cairo_fibers_request_end(void);

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_fiber_context, 0, 0, 0)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_fiber_bind, 0, 0, 3)
	ZEND_ARG_INFO(0, fiber)
	ZEND_ARG_INFO(0, context)
	ZEND_ARG_INFO(0, request)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_fiber_reuse, 0, 0, 3)
	ZEND_ARG_INFO(0, context)
	ZEND_ARG_INFO(0, request)
	ZEND_ARG_INFO(0, job)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_fiber_idle, 0, 0, 0)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_ready, 0, 0, 1)
	ZEND_ARG_INFO(0, fiber)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_run_ready, 0, 0, 1)
	ZEND_ARG_INFO(0, max)
ZEND_END_ARG_INFO()

PHP_FUNCTION(cairo_fiber_context);
PHP_FUNCTION(cairo_fiber_bind);
PHP_FUNCTION(cairo_fiber_reuse);
PHP_FUNCTION(cairo_fiber_idle);
PHP_FUNCTION(cairo_fiber_alone);
PHP_FUNCTION(cairo_ready);
PHP_FUNCTION(cairo_run_ready);

#endif

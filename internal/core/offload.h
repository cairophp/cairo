#ifndef CAIRO_OFFLOAD_H
#define CAIRO_OFFLOAD_H

#include <stdint.h>

#include <php.h>

#include "queue.h"

// The way of an offload() job (offload.c). The request thread whose fiber
// asks queues the job straight into the inbox of an offload thread, and that
// thread queues the result straight into the inbox of the request thread:
// two wake-ups of threads, and no goroutine. Go names the offload threads
// and speaks for one that ended in the middle of a job.

// How many events may wait for one offload thread before a job for it waits
// (cairo_offload_send() answers 0).
#define CAIRO_OFFLOAD_QUEUE 4096

// The offload threads have these n thread ids from now on. Call it before
// the PHP threads start. Returns -1 when there is no memory for them.
int cairo_offload_pool(const int *tids, int n);

// Thread tid took m out of its inbox, a job (CAIRO_EVENT_JOB) or the result
// of one (CAIRO_EVENT_DONE): fills event with what PHP reads of it, and
// keeps whom a job is for until cairo_offload_done(). Returns SUCCESS, or
// FAILURE for a message that is neither.
int cairo_offload_event(int tid, const cairo_msg *m, zval *event);

// Offload thread tid has ended. Returns the job it had taken and not
// answered, which nobody will answer now, with the thread that waits for it
// in *from; 0 when it had none.
int64_t cairo_offload_lost(int tid, int *from);

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_offload_send, 0, 0, 2)
	ZEND_ARG_INFO(0, job)
	ZEND_ARG_INFO(0, request)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_offload_done, 0, 0, 1)
	ZEND_ARG_INFO(0, result)
ZEND_END_ARG_INFO()

PHP_FUNCTION(cairo_offload_send);
PHP_FUNCTION(cairo_offload_done);

#endif

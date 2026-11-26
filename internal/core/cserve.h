#ifndef CAIRO_CSERVE_H
#define CAIRO_CSERVE_H

#include <stdbool.h>

#include <php.h>

#include "extension.h"

// A request thread whose requests an extension serves in C (its
// cairo_cserver, extension.h), as PHP's own SAPIs serve theirs: no PHP of
// Cairo's runs around the request. The thread turns it on from PHP
// (cairo_cserve()), and from its next PHP request on, it takes its call in
// C before PHP starts the request (cairo_thread_run() in cairo.c), so that
// PHP's own code fills $_GET, $_POST, $_COOKIE and $_FILES from what the
// server gives it. An event that is not a call is the runtime's: that PHP
// request runs the runtime as any other, which takes the event.

// cairo_cserve(): bool
PHP_FUNCTION(cairo_cserve);

// Takes the calling thread's next event for the PHP request that it starts
// next: true when it is a call that the extension serves in C, which has
// begun it (its begin), false when that PHP request runs the runtime.
// Before cairo_startgate_startup().
bool cairo_cserve_take(int tid);

// The server, while a PHP request that it serves runs on the calling
// thread, from its start to the end of its shutdown; NULL otherwise.
const cairo_cserver *cairo_cserving(void);

// The PHP request taken with cairo_cserve_take() has ended: the server
// forgets it, and the call's event is freed.
void cairo_cserve_done(void);

// A thread that starts forgets what a former thread on the same system
// thread turned on (as cairo_classic_forget()).
void cairo_cserve_forget(void);

#endif

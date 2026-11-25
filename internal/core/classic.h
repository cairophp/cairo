#ifndef CAIRO_CLASSIC_H
#define CAIRO_CLASSIC_H

#include <stdbool.h>

#include <php.h>

// Classic PHP (App::concurrency('classic')): each request has a PHP request
// of its own, as under PHP-FPM, for apps that are a set of scripts. A request
// thread runs one PHP request for its whole life; in classic mode, once its
// request is over, it starts the next PHP request on the same system thread
// (cairo_thread_run() in cairo.c), which keeps its PHP thread resources:
// copying PHP's function, class and constant tables for a new thread cost
// four times what the PHP request does.

// The PHP request of the calling thread has ended (php_request_shutdown()):
// what the core and the extensions keep of it in thread-local variables,
// which was the request's memory, is forgotten, for a thread that ends or
// starts its next PHP request.
void cairo_request_ended(void);

// Whether the PHP request that just ended asked for the next one
// (cairo_thread_again()), and forgets it.
bool cairo_classic_again(void);

// cairo_thread_again(): void
PHP_FUNCTION(cairo_thread_again);

// A classic thread whose app serves only scripts() checked its declaration
// in its first PHP request (cairo_thread_check_passed()): its later PHP
// requests run server.php without sending the declaration to Go. Unlike the
// rest of classic.c this outlives the PHP request, so a thread that starts
// forgets it first (cairo_classic_forget()): the system thread may have been
// another PHP thread's, from before a reload.
void cairo_classic_forget(void);

// cairo_thread_checked(): bool
PHP_FUNCTION(cairo_thread_checked);

// cairo_thread_check_passed(): void
PHP_FUNCTION(cairo_thread_check_passed);

// cairo_run_script(string $file): bool
PHP_FUNCTION(cairo_run_script);

#endif

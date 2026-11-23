#ifndef CAIRO_SOCKETS_H
#define CAIRO_SOCKETS_H

#include <stdbool.h>
#include <stdint.h>

#include <php.h>

// The sockets that fibers of a PHP thread wait on (sockets.c). Each thread
// has an epoll of its own: a fiber arms a socket in it and parks on the
// thread, in one call (cairo_wait_socket(), which Waits in waits.php
// calls), and the thread's loop asks the epoll which sockets are ready, once
// a turn, or sleeps in it when it has nothing to run, and wakes their
// fibers. Neither Go nor PHP code is involved in between.

// What a fiber that waits here waits in, for the probe of its request: the
// constants SOCKET and SOCKETS of Waits.
#define CAIRO_WAIT_SOCKET 2
#define CAIRO_WAIT_SOCKETS 3

// How many sockets fibers of the calling thread wait on.
int cairo_sockets_armed(void);

// Wakes the fiber of the next socket that the calling thread found ready,
// or of all of them, and returns how many it woke: they run at the loop's
// next turn.
int cairo_sockets_take(bool all);

// Asks the kernel which sockets are ready, and waits for one for ns
// nanoseconds at most (0: not at all; below 0: as long as it takes), or
// until the thread is woken (cairo_sockets_wake). Returns whether
// cairo_sockets_take() has a socket.
int cairo_sockets_wait(int64_t ns);

// Wakes the thread whose mailbox holds waker from cairo_sockets_wait().
void cairo_sockets_wake(int32_t waker);

// The calling thread ends: its epoll is closed.
void cairo_sockets_thread_end(void);

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_wait_socket, 0, 0, 6)
	ZEND_ARG_INFO(0, fd)
	ZEND_ARG_INFO(0, events)
	ZEND_ARG_INFO(0, id)
	ZEND_ARG_INFO(0, context)
	ZEND_ARG_INFO(0, request)
	ZEND_ARG_INFO(0, at)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_wait_sockets, 0, 0, 5)
	ZEND_ARG_INFO(0, events)
	ZEND_ARG_INFO(0, id)
	ZEND_ARG_INFO(0, context)
	ZEND_ARG_INFO(0, request)
	ZEND_ARG_INFO(0, at)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_wait_here, 0, 0, 2)
	ZEND_ARG_INFO(0, fd)
	ZEND_ARG_INFO(0, write)
ZEND_END_ARG_INFO()

PHP_FUNCTION(cairo_wait_socket);
PHP_FUNCTION(cairo_wait_sockets);
PHP_FUNCTION(cairo_wait_here);

#endif

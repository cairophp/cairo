#ifndef CAIRO_SOCKETS_H
#define CAIRO_SOCKETS_H

#include <stdint.h>

#include <php.h>

// The sockets that fibers of a PHP thread wait on (sockets.c). Each thread
// has an epoll of its own: a fiber arms a socket in it and parks on the
// thread (Waits in waits.php), and the thread's loop asks the epoll which
// sockets are ready, once a turn, or sleeps in it when it has nothing to
// run. Go is not involved.

// How many sockets fibers of the calling thread wait on.
int cairo_sockets_armed(void);

// The next socket that the calling thread found ready, or -1.
int cairo_sockets_next(void);

// Asks the kernel which sockets are ready, and waits for one for ns
// nanoseconds at most (0: not at all; below 0: as long as it takes), or
// until the thread is woken (cairo_sockets_wake). Returns whether
// cairo_sockets_next() has a socket.
int cairo_sockets_wait(int64_t ns);

// Wakes the thread whose mailbox holds waker from cairo_sockets_wait().
void cairo_sockets_wake(int32_t waker);

// The calling thread ends: its epoll is closed.
void cairo_sockets_thread_end(void);

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_socket_arm, 0, 0, 2)
	ZEND_ARG_INFO(0, fd)
	ZEND_ARG_INFO(0, events)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_socket_disarm, 0, 0, 1)
	ZEND_ARG_INFO(0, fd)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_wait_here, 0, 0, 2)
	ZEND_ARG_INFO(0, fd)
	ZEND_ARG_INFO(0, write)
ZEND_END_ARG_INFO()

PHP_FUNCTION(cairo_socket_arm);
PHP_FUNCTION(cairo_socket_disarm);
PHP_FUNCTION(cairo_wait_here);

#endif

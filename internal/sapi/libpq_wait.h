#ifndef CAIRO_LIBPQ_WAIT_H
#define CAIRO_LIBPQ_WAIT_H

#include <poll.h>

#include "extension.h"

// poll() for libpq's calls: inside a request fiber the fiber waits, on the
// thread's epoll, and everywhere else the thread does (libpq_wait.c).
int cairo_libpq_poll(struct pollfd *fds, nfds_t nfds, int timeout);

// Looks for libpq once PHP's startup has loaded every extension; from then
// on poll() looks no more (libpq_wait.c).
void cairo_libpq_settle(void);

// Whether the calling fiber is in pdo_pgsql's getNotify(), whose poll() is
// its own wait.
int cairo_pgsql_notify_wait(void);

// PHP's pdo_pgsql as Cairo has it (pdo_pgsql_defaults.c): one round trip a
// query unless the app asks for prepared statements, and getNotify() with
// a timeout waits on the fiber. pgsql.go adds it to the SAPI module.
extern const cairo_extension cairo_pdo_pgsql;

#endif

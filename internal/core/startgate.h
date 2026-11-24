#ifndef CAIRO_STARTGATE_H
#define CAIRO_STARTGATE_H

// A PHP thread that starts while others run, and opcache.
//
// opcache restarts its cache (it is full, or the app called opcache_reset())
// at the start of the next PHP request, once it finds that no process runs
// cached code. On Linux it asks the file locks that its requests hold, which
// are a process's: in a process of threads, it finds nobody, and wipes the
// cache under the threads that run code from it. A thread of Cairo is one
// request of PHP for its whole life, so that next request is the next
// thread that starts: a recycled one, or one that takes a dead one's place.
//
// So from the moment opcache schedules a restart, which it tells through a
// hook since PHP 8.4, no thread starts here any more: the restart stays pending and the
// threads that run are safe. The process has to start anew to get its cache
// back; the engine reports that (ErrRestartNeeded in supervise.go).
// An older PHP has no hook, and no thread is refused there.

// What cairo_thread_run() returns for a thread that was not let in.
#define CAIRO_START_REFUSED (-2)

// Takes opcache's hook. Once, after PHP's modules have started.
void cairo_startgate_init(void);

// php_request_startup() for a thread that starts while others may run: 0,
// -1 when PHP could not start the request, or CAIRO_START_REFUSED.
int cairo_startgate_startup(void);

// Why opcache scheduled its restart, for the log. "" when it has not.
const char *cairo_startgate_reason(void);

#endif

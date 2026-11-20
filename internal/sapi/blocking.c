// App::detectBlocking(): the report a PHP thread makes of the code that kept
// it busy. Go's detector (blocking.go) says for how long and interrupts the
// thread, which makes the report in whatever fiber is running.

#include <stdatomic.h>

#include <php.h>

#include "blocking.h"

// By thread id; > 0: report that the running code blocked the thread this long.
static atomic_int blocked_ms[CAIRO_MAX_THREADS];

void cairo_report_blocking(int tid, int ms)
{
	if (tid >= 0 && tid < CAIRO_MAX_THREADS) {
		atomic_store(&blocked_ms[tid], ms);
	}
}

// The thread's interrupt: Cairo\Internal\reportBlocking() logs where the
// fiber is.
static void cairo_blocking_interrupt(int tid)
{
	int ms = atomic_exchange(&blocked_ms[tid], 0);
	if (ms <= 0) {
		return;
	}
	zval fn, ret, arg;
	ZVAL_STRING(&fn, "Cairo\\Internal\\reportBlocking");
	ZVAL_LONG(&arg, ms);
	ZVAL_UNDEF(&ret);
	call_user_function(NULL, NULL, &fn, &ret, 1, &arg);
	zval_ptr_dtor(&fn);
	zval_ptr_dtor(&ret);
}

const cairo_extension cairo_blocking = {.interrupt = cairo_blocking_interrupt};

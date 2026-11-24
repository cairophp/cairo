// A PHP thread that starts while others run, and opcache. See startgate.h.

#include <pthread.h>

#include <php.h>
#include <main/php_main.h>

#include "startgate.h"

// Held by a thread through its request startup, and by the hook while it
// notes the restart: opcache sets the restart pending after the hook has
// returned, so a thread is either refused or through its startup by then.
static pthread_mutex_t gate = PTHREAD_MUTEX_INITIALIZER;
static const char *scheduled;        // why, under gate; NULL: no restart is pending
#if PHP_VERSION_ID >= 80400
static void (*hooked)(int reason);   // a hook that was there before this one
#endif
static __thread int starting;        // this thread holds gate

// opcache tells of the restart since PHP 8.4. An older PHP gives no sign of
// it: there the gate lets every thread start, and the cache is wiped under
// the threads that run, as it was before there was a gate.
#if PHP_VERSION_ID >= 80400
static void restart_scheduled(int reason)
{
	// opcache's zend_accel_restart_reason: out of memory, hash overflow, user.
	static const char *const reasons[] = {
		"opcache's memory is full (opcache.memory_consumption)",
		"opcache has no room for more files (opcache.max_accelerated_files)",
		"opcache_reset() was called",
	};
	const char *why = reason >= 0 && reason < 3 ? reasons[reason] : "opcache asked for it";
	if (starting) {
		scheduled = why;
	} else {
		pthread_mutex_lock(&gate);
		scheduled = why;
		pthread_mutex_unlock(&gate);
	}
	if (hooked != NULL) {
		hooked(reason);
	}
}
#endif

void cairo_startgate_init(void)
{
#if PHP_VERSION_ID >= 80400
	hooked = zend_accel_schedule_restart_hook;
	zend_accel_schedule_restart_hook = restart_scheduled;
#endif
}

int cairo_startgate_startup(void)
{
	pthread_mutex_lock(&gate);
	int rc = CAIRO_START_REFUSED;
	if (scheduled == NULL) {
		starting = 1;
		rc = php_request_startup() == FAILURE ? -1 : 0;
		starting = 0;
	}
	pthread_mutex_unlock(&gate);
	return rc;
}

const char *cairo_startgate_reason(void)
{
	pthread_mutex_lock(&gate);
	const char *why = scheduled != NULL ? scheduled : "";
	pthread_mutex_unlock(&gate);
	return why;
}

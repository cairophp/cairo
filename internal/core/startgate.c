// A PHP thread that starts while others run, and opcache. See startgate.h.

#include <pthread.h>
#include <string.h>

#include <php.h>
#include <main/php_main.h>

#include "startgate.h"

// Held by a thread through its request startup, and by the hook while it
// notes the restart: opcache sets the restart pending after the hook has
// returned, so a thread is either refused or through its startup by then.
static pthread_mutex_t gate = PTHREAD_MUTEX_INITIALIZER;
static const char *scheduled;        // why, under gate; NULL: no restart is pending
static void (*hooked)(int reason);   // a hook that was there before this one
static __thread int starting;        // this thread holds gate

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

void cairo_startgate_init(void)
{
	hooked = zend_accel_schedule_restart_hook;
	zend_accel_schedule_restart_hook = restart_scheduled;
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

PHP_FUNCTION(cairo_restart)
{
	char *why;
	size_t len;
	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_STRING(why, len)
	ZEND_PARSE_PARAMETERS_END();
	pthread_mutex_lock(&gate);
	if (scheduled == NULL) {
		char *kept = strndup(why, len);   // for the life of the process
		scheduled = kept != NULL ? kept : "a reload asked for it";
	}
	pthread_mutex_unlock(&gate);
}

const char *cairo_startgate_reason(void)
{
	pthread_mutex_lock(&gate);
	const char *why = scheduled != NULL ? scheduled : "";
	pthread_mutex_unlock(&gate);
	return why;
}

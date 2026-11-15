// Cairo's bridge between Go and an embedded, thread-safe (ZTS) PHP.
//
// Milestone 1 builds on PHP's own embed SAPI: one boot thread runs the
// declare phase, then each PHP worker thread gets its own interpreter context
// (ts_resource) and runs the Cairo runtime script, which loops on
// cairo_next_event() and runs every request in its own Fiber.

#include <pthread.h>
#include <stdlib.h>
#include <string.h>

#include <php.h>
#include <php_main.h>
#include <SAPI.h>
#include <TSRM.h>
#include <zend_exceptions.h>
#include <zend_stream.h>
#include <sapi/embed/php_embed.h>

#include "cairo.h"
#include "_cgo_export.h"

// -1 is the boot thread (declare phase); 0..n-1 are worker threads.
static __thread int cairo_tid = -1;

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_none, 0, 0, 0)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_spec, 0, 0, 1)
	ZEND_ARG_INFO(0, json)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_respond, 0, 0, 2)
	ZEND_ARG_INFO(0, id)
	ZEND_ARG_INFO(0, json)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_fiber_open, 0, 0, 1)
	ZEND_ARG_INFO(0, parent)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_fiber_close, 0, 0, 1)
	ZEND_ARG_INFO(0, fiber)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_fiber_op, 0, 0, 2)
	ZEND_ARG_INFO(0, fiber)
	ZEND_ARG_INFO(0, json)
ZEND_END_ARG_INFO()

// cairo_info(): string (JSON: mode, server file, thread id)
PHP_FUNCTION(cairo_info)
{
	ZEND_PARSE_PARAMETERS_NONE();
	char *s = goCairoInfo(cairo_tid);
	RETVAL_STRING(s);
	free(s);
}

// cairo_spec(string $json): void. Declare phase: records the spec.
// Serve phase: checks this thread built the same spec as the boot thread.
PHP_FUNCTION(cairo_spec)
{
	char *json;
	size_t len;
	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_STRING(json, len)
	ZEND_PARSE_PARAMETERS_END();
	char *err = goCairoSpec(cairo_tid, json, (int)len);
	if (err != NULL) {
		zend_throw_exception(NULL, err, 0);
		free(err);
	}
}

// cairo_next_event(): string (JSON). Blocks this PHP thread until Go has
// work for it: a new request, a finished I/O operation, or shutdown.
PHP_FUNCTION(cairo_next_event)
{
	ZEND_PARSE_PARAMETERS_NONE();
	char *s = goNextEvent(cairo_tid);
	RETVAL_STRING(s);
	free(s);
}

// cairo_respond(int $id, string $json): void
PHP_FUNCTION(cairo_respond)
{
	zend_long id;
	char *json;
	size_t len;
	ZEND_PARSE_PARAMETERS_START(2, 2)
		Z_PARAM_LONG(id)
		Z_PARAM_STRING(json, len)
	ZEND_PARSE_PARAMETERS_END();
	goRespond((long long)id, json, (int)len);
}

// cairo_fiber_open(int $parentFiber): int. Gives a new fiber its own
// goroutine, with a child of the parent fiber's context.
PHP_FUNCTION(cairo_fiber_open)
{
	zend_long parent;
	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_LONG(parent)
	ZEND_PARSE_PARAMETERS_END();
	RETURN_LONG((zend_long)goFiberOpen(cairo_tid, (long long)parent));
}

// cairo_fiber_close(int $fiber): void
PHP_FUNCTION(cairo_fiber_close)
{
	zend_long fiber;
	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_LONG(fiber)
	ZEND_PARSE_PARAMETERS_END();
	goFiberClose((long long)fiber);
}

// cairo_fiber_op(int $fiber, string $json): int. Asks the fiber's goroutine
// to perform an operation; returns its id (or -1). The caller then suspends.
PHP_FUNCTION(cairo_fiber_op)
{
	zend_long fiber;
	char *json;
	size_t len;
	ZEND_PARSE_PARAMETERS_START(2, 2)
		Z_PARAM_LONG(fiber)
		Z_PARAM_STRING(json, len)
	ZEND_PARSE_PARAMETERS_END();
	RETURN_LONG((zend_long)goFiberOp((long long)fiber, json, (int)len));
}

static const zend_function_entry cairo_functions[] = {
	PHP_FE(cairo_info, arginfo_cairo_none)
	PHP_FE(cairo_spec, arginfo_cairo_spec)
	PHP_FE(cairo_next_event, arginfo_cairo_none)
	PHP_FE(cairo_respond, arginfo_cairo_respond)
	PHP_FE(cairo_fiber_open, arginfo_cairo_fiber_open)
	PHP_FE(cairo_fiber_close, arginfo_cairo_fiber_close)
	PHP_FE(cairo_fiber_op, arginfo_cairo_fiber_op)
	PHP_FE_END
};

// ---- Runtime hooks -------------------------------------------------------
//
// Cairo swaps the handlers of PHP's blocking sleep functions. Inside a fiber,
// the new handler asks the PHP runtime (Cairo\Internal\Hooks::sleep) to
// suspend the fiber while its goroutine waits; PHP fibers are stackful, so
// the fiber can suspend from inside this C function. Outside a fiber, or when
// the runtime declines (hooks off, offload thread, a fiber Cairo did not
// start), the original handler runs and blocks as usual.

enum { HOOK_SLEEP, HOOK_USLEEP, HOOK_NANOSLEEP, HOOK_COUNT };

static const char *hook_names[HOOK_COUNT] = {"sleep", "usleep", "time_nanosleep"};
static zif_handler hook_originals[HOOK_COUNT];

// Returns 1 when the runtime slept for us (or threw), 0 to fall back.
static int cairo_hooked_sleep(double seconds)
{
	if (EG(active_fiber) == NULL) {
		return 0;
	}
	zval fname, arg, retval;
	ZVAL_STRING(&fname, "Cairo\\Internal\\Hooks::sleep");
	ZVAL_DOUBLE(&arg, seconds);
	ZVAL_UNDEF(&retval);
	int rc = call_user_function(NULL, NULL, &fname, &retval, 1, &arg);
	zval_ptr_dtor(&fname);
	if (rc != SUCCESS) {
		return EG(exception) != NULL;
	}
	if (EG(exception) != NULL) {
		zval_ptr_dtor(&retval);
		return 1;
	}
	int handled = zend_is_true(&retval);
	zval_ptr_dtor(&retval);
	return handled;
}

static ZEND_NAMED_FUNCTION(cairo_hook_sleep)
{
	zend_long seconds;
	if (ZEND_NUM_ARGS() == 1 && Z_TYPE_P(ZEND_CALL_ARG(execute_data, 1)) == IS_LONG) {
		seconds = Z_LVAL_P(ZEND_CALL_ARG(execute_data, 1));
		if (seconds >= 0 && cairo_hooked_sleep((double)seconds)) {
			if (EG(exception) == NULL) {
				RETVAL_LONG(0);
			}
			return;
		}
	}
	hook_originals[HOOK_SLEEP](INTERNAL_FUNCTION_PARAM_PASSTHRU);
}

static ZEND_NAMED_FUNCTION(cairo_hook_usleep)
{
	if (ZEND_NUM_ARGS() == 1 && Z_TYPE_P(ZEND_CALL_ARG(execute_data, 1)) == IS_LONG) {
		zend_long us = Z_LVAL_P(ZEND_CALL_ARG(execute_data, 1));
		if (us >= 0 && cairo_hooked_sleep((double)us / 1e6)) {
			return;
		}
	}
	hook_originals[HOOK_USLEEP](INTERNAL_FUNCTION_PARAM_PASSTHRU);
}

static ZEND_NAMED_FUNCTION(cairo_hook_nanosleep)
{
	if (ZEND_NUM_ARGS() == 2
		&& Z_TYPE_P(ZEND_CALL_ARG(execute_data, 1)) == IS_LONG
		&& Z_TYPE_P(ZEND_CALL_ARG(execute_data, 2)) == IS_LONG) {
		zend_long s = Z_LVAL_P(ZEND_CALL_ARG(execute_data, 1));
		zend_long ns = Z_LVAL_P(ZEND_CALL_ARG(execute_data, 2));
		if (s >= 0 && ns >= 0 && ns <= 999999999 && cairo_hooked_sleep((double)s + (double)ns / 1e9)) {
			if (EG(exception) == NULL) {
				RETVAL_TRUE;
			}
			return;
		}
	}
	hook_originals[HOOK_NANOSLEEP](INTERNAL_FUNCTION_PARAM_PASSTHRU);
}

static const zif_handler hook_handlers[HOOK_COUNT] = {cairo_hook_sleep, cairo_hook_usleep, cairo_hook_nanosleep};

// Module startup: install the hooks once, before any worker thread copies
// the function table.
static PHP_MINIT_FUNCTION(cairo)
{
	for (int i = 0; i < HOOK_COUNT; i++) {
		zend_function *fn = zend_hash_str_find_ptr(CG(function_table), hook_names[i], strlen(hook_names[i]));
		if (fn != NULL && fn->type == ZEND_INTERNAL_FUNCTION) {
			hook_originals[i] = fn->internal_function.handler;
			fn->internal_function.handler = hook_handlers[i];
		}
	}
	return SUCCESS;
}

static zend_module_entry cairo_module_entry = {
	STANDARD_MODULE_HEADER,
	"cairo",
	cairo_functions,
	PHP_MINIT(cairo), NULL, NULL, NULL, NULL,
	"0.1.0-dev",
	STANDARD_MODULE_PROPERTIES
};

// Replaces the embed SAPI's startup hook so the cairo module (and its
// functions) is registered during module startup, like any PHP extension.
static int cairo_sapi_startup(sapi_module_struct *sapi)
{
	return php_module_startup(sapi, &cairo_module_entry);
}

static int run_file(const char *file)
{
	zend_file_handle fh;
	zend_stream_init_filename(&fh, file);
	bool ok = php_execute_script(&fh);
	zend_destroy_file_handle(&fh);
	if (!ok) {
		return -1;
	}
	return EG(exit_status);
}

int cairo_boot(const char *runtime_file)
{
	static char *argv[] = {"cairo", NULL};
	php_embed_module.startup = cairo_sapi_startup;
	if (php_embed_init(1, argv) == FAILURE) {
		return -1;
	}
	cairo_tid = -1;
	return run_file(runtime_file);
}

void cairo_shutdown(void)
{
	php_embed_shutdown();
}

int cairo_thread_run(int tid, const char *runtime_file)
{
	cairo_tid = tid;
	(void)ts_resource(0);
	if (php_request_startup() == FAILURE) {
		ts_free_thread();
		return -1;
	}
	int status = run_file(runtime_file);
	php_request_shutdown(NULL);
	ts_free_thread();
	return status;
}

// PHP worker threads are created here in C, not by Go. PHP runs each fiber
// on its own C stack, and Go only accepts callbacks from a foreign stack on
// threads it did not create, so cairo_* functions called inside a fiber work.
struct cairo_thread_arg {
	int tid;
	char *file;
};

static void *cairo_thread_main(void *p)
{
	struct cairo_thread_arg *arg = p;
	int status = cairo_thread_run(arg->tid, arg->file);
	int tid = arg->tid;
	free(arg->file);
	free(arg);
	goThreadDone(tid, status);
	return NULL;
}

int cairo_thread_start(int tid, const char *runtime_file)
{
	struct cairo_thread_arg *arg = malloc(sizeof(*arg));
	if (arg == NULL) {
		return -1;
	}
	arg->tid = tid;
	arg->file = strdup(runtime_file);

	pthread_attr_t attr;
	pthread_attr_init(&attr);
	pthread_attr_setstacksize(&attr, 16 * 1024 * 1024);
	pthread_attr_setdetachstate(&attr, PTHREAD_CREATE_DETACHED);
	pthread_t th;
	int rc = pthread_create(&th, &attr, cairo_thread_main, arg);
	pthread_attr_destroy(&attr);
	if (rc != 0) {
		free(arg->file);
		free(arg);
		return -1;
	}
	return 0;
}

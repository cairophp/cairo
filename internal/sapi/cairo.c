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

static zend_module_entry cairo_module_entry = {
	STANDARD_MODULE_HEADER,
	"cairo",
	cairo_functions,
	NULL, NULL, NULL, NULL, NULL,
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

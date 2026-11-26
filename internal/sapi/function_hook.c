// Runtime hooks: PHP functions whose handlers Cairo swaps.
//
// Cairo swaps the handlers of PHP's blocking sleep functions. Inside a fiber,
// the new handler asks the PHP runtime (Cairo\Internal\Hooks::sleep) to
// suspend the fiber until its time is up; PHP fibers are stackful, so
// the fiber can suspend from inside this C function. Outside a fiber, or when
// the runtime declines (hooks off, offload thread, a fiber Cairo did not
// start), the original handler runs and blocks as usual.

#include <errno.h>
#include <php.h>
#include "hooks.h"

enum {
	HOOK_SLEEP, HOOK_USLEEP, HOOK_NANOSLEEP, HOOK_GETHOSTBYNAME, HOOK_GETHOSTBYNAMEL,
	HOOK_IS_UPLOADED_FILE, HOOK_MOVE_UPLOADED_FILE, HOOK_COUNT
};

static const char *hook_names[HOOK_COUNT] = {
	"sleep", "usleep", "time_nanosleep", "gethostbyname", "gethostbynamel",
	"is_uploaded_file", "move_uploaded_file",
};
static zif_handler hook_originals[HOOK_COUNT];

// A sleep outside any fiber, of a classic request on its thread's own stack:
// one that would last past the request's deadline ends there, with
// Context\DeadlineExceeded. Returns as cairo_hooked_sleep() does.
static int sleep_on_stack(double seconds)
{
	int64_t at = cairo_stack_deadline();
	if (at == 0 || seconds * 1e9 < (double)(at - cairo_mono_ns())) {
		return 0;
	}
	struct timespec until = {.tv_sec = at / 1000000000, .tv_nsec = at % 1000000000};
	while (clock_nanosleep(CLOCK_MONOTONIC, TIMER_ABSTIME, &until, NULL) == EINTR) {
	}
	cairo_throw_deadline();
	return 1;
}

// Returns 1 when the runtime slept for us (or threw), 0 to fall back.
static int cairo_hooked_sleep(double seconds)
{
	if (EG(active_fiber) == NULL) {
		return sleep_on_stack(seconds);
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

// Forwards a call to Cairo\Internal\Hooks::<name>(...$args). A null result
// means the runtime declined, and the original function runs instead.
static int forward(const char *method, zend_execute_data *execute_data, zval *return_value)
{
	char name[96];
	snprintf(name, sizeof(name), "Cairo\\Internal\\Hooks::%s", method);
	zval fname, retval;
	ZVAL_STRING(&fname, name);
	ZVAL_UNDEF(&retval);
	uint32_t argc = ZEND_NUM_ARGS();
	int rc = call_user_function(NULL, NULL, &fname, &retval, argc, argc ? ZEND_CALL_ARG(execute_data, 1) : NULL);
	zval_ptr_dtor(&fname);
	if (EG(exception) != NULL) {
		zval_ptr_dtor(&retval);
		return 1;
	}
	if (rc != SUCCESS || Z_TYPE(retval) == IS_NULL || Z_TYPE(retval) == IS_UNDEF) {
		zval_ptr_dtor(&retval);
		return 0;
	}
	ZVAL_COPY_VALUE(return_value, &retval);
	return 1;
}

// What waits is the runtime's only inside a fiber.
static int cairo_forward(const char *method, zend_execute_data *execute_data, zval *return_value)
{
	return EG(active_fiber) != NULL && forward(method, execute_data, return_value);
}

// Cairo's uploads are a request's wherever it runs, on its thread's own stack
// too (classic mode). Where Cairo parsed none (Cairo\Internal\Form is not
// loaded), the uploads are PHP's own.
static int cairo_forward_upload(const char *method, zend_execute_data *execute_data, zval *return_value)
{
	return zend_hash_str_exists(EG(class_table), "cairo\\internal\\form", sizeof("cairo\\internal\\form") - 1)
		&& forward(method, execute_data, return_value);
}

static ZEND_NAMED_FUNCTION(cairo_hook_gethostbyname)
{
	if (!cairo_forward("gethostbyname", execute_data, return_value)) {
		hook_originals[HOOK_GETHOSTBYNAME](INTERNAL_FUNCTION_PARAM_PASSTHRU);
	}
}

static ZEND_NAMED_FUNCTION(cairo_hook_gethostbynamel)
{
	if (!cairo_forward("gethostbynamel", execute_data, return_value)) {
		hook_originals[HOOK_GETHOSTBYNAMEL](INTERNAL_FUNCTION_PARAM_PASSTHRU);
	}
}

// Uploads are parsed by Go, so PHP's own list of uploaded files is empty;
// these ask the runtime about Cairo's uploads first.
static ZEND_NAMED_FUNCTION(cairo_hook_is_uploaded_file)
{
	if (!cairo_forward_upload("isUploadedFile", execute_data, return_value)) {
		hook_originals[HOOK_IS_UPLOADED_FILE](INTERNAL_FUNCTION_PARAM_PASSTHRU);
	}
}

static ZEND_NAMED_FUNCTION(cairo_hook_move_uploaded_file)
{
	if (!cairo_forward_upload("moveUploadedFile", execute_data, return_value)) {
		hook_originals[HOOK_MOVE_UPLOADED_FILE](INTERNAL_FUNCTION_PARAM_PASSTHRU);
	}
}

static const zif_handler hook_handlers[HOOK_COUNT] = {
	cairo_hook_sleep, cairo_hook_usleep, cairo_hook_nanosleep,
	cairo_hook_gethostbyname, cairo_hook_gethostbynamel,
	cairo_hook_is_uploaded_file, cairo_hook_move_uploaded_file,
};

// Installs the hooks at module startup, before any worker thread copies the
// function table.
static void cairo_hook_functions(void)
{
	for (int i = 0; i < HOOK_COUNT; i++) {
		zend_function *fn = zend_hash_str_find_ptr(CG(function_table), hook_names[i], strlen(hook_names[i]));
		if (fn != NULL && fn->type == ZEND_INTERNAL_FUNCTION) {
			hook_originals[i] = fn->internal_function.handler;
			fn->internal_function.handler = hook_handlers[i];
		}
	}
}

const cairo_extension cairo_function_hooks = {.startup = cairo_hook_functions};

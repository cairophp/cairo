// Cairo's concurrency functions are C functions of the module, so that a
// PHP request has them without loading a file: a script served with no
// runtime (internal/sapi/plain_request.c) pays nothing for them until it
// calls one. Each forwards to its PHP in Cairo\Internal\Fn (the core's PHP
// files), and its first call where that is not loaded yet loads it, by the
// autoloading of the scheduler's class. The function is the same one in
// every thread and mode, so what opcache compiled against it holds in all.

#include <php.h>
#include <zend_exceptions.h>
#include <zend_interfaces.h>

#include "api.h"

// Calls the PHP of name in Cairo\Internal\Fn with this call's arguments.
static void forward(const char *name, size_t len, zend_execute_data *execute_data, zval *return_value)
{
	zend_function *fn = zend_hash_str_find_ptr(EG(function_table), name, len);
	if (fn == NULL) {
		zend_string *scheduler = zend_string_init("Cairo\\Internal\\Scheduler", sizeof("Cairo\\Internal\\Scheduler") - 1, 0);
		zend_lookup_class(scheduler);   // loads the runtime where it is not loaded yet
		zend_string_release(scheduler);
		if (EG(exception) != NULL) {
			return;
		}
		fn = zend_hash_str_find_ptr(EG(function_table), name, len);
		if (fn == NULL) {
			zend_throw_error(NULL, "Cairo's runtime is not loaded");
			return;
		}
	}
	zend_fcall_info fci = {
		.size = sizeof(zend_fcall_info),
		.retval = return_value,
		.params = ZEND_CALL_NUM_ARGS(execute_data) ? ZEND_CALL_ARG(execute_data, 1) : NULL,
		.param_count = ZEND_CALL_NUM_ARGS(execute_data),
		.named_params = ZEND_CALL_INFO(execute_data) & ZEND_CALL_HAS_EXTRA_NAMED_PARAMS ? execute_data->extra_named_params : NULL,
	};
	ZVAL_UNDEF(&fci.function_name);
	zend_fcall_info_cache fcc = {.function_handler = fn};
	zend_call_function(&fci, &fcc);
}

#define FORWARD(name) \
	static ZEND_FUNCTION(cairo_api_##name) \
	{ \
		forward("cairo\\internal\\fn\\" #name, sizeof("cairo\\internal\\fn\\" #name) - 1, execute_data, return_value); \
	}

FORWARD(async)
FORWARD(await)
FORWARD(all)
FORWARD(sleep)
FORWARD(select)
FORWARD(offload)

// The signatures of the PHP functions in internal/core/php (Reflection and
// named arguments read these); test/api_test.go compares them.
ZEND_BEGIN_ARG_WITH_RETURN_OBJ_INFO_EX(arginfo_async, 0, 1, Cairo\\Task, 0)
	ZEND_ARG_TYPE_INFO(0, fn, IS_CALLABLE, 0)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_WITH_RETURN_TYPE_INFO_EX(arginfo_await, 0, 1, IS_MIXED, 0)
	ZEND_ARG_OBJ_INFO(0, task, Cairo\\Task, 0)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_WITH_RETURN_OBJ_INFO_EX(arginfo_all, 0, 1, Cairo\\Task, 0)
	ZEND_ARG_TYPE_INFO(0, tasks, IS_ARRAY, 0)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_WITH_RETURN_TYPE_INFO_EX(arginfo_sleep, 0, 1, IS_VOID, 0)
	ZEND_ARG_TYPE_INFO(0, seconds, IS_DOUBLE, 0)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_WITH_RETURN_TYPE_INFO_EX(arginfo_select, 0, 1, IS_ARRAY, 0)
	ZEND_ARG_TYPE_INFO(0, cases, IS_ARRAY, 0)
	ZEND_ARG_TYPE_INFO_WITH_DEFAULT_VALUE(0, timeout, IS_DOUBLE, 1, "null")
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_WITH_RETURN_TYPE_INFO_EX(arginfo_offload, 0, 1, IS_MIXED, 0)
	ZEND_ARG_OBJ_TYPE_MASK(0, fn, Cairo\\OffloadTask, MAY_BE_STRING|MAY_BE_ARRAY, NULL)
	ZEND_ARG_VARIADIC_TYPE_INFO(0, args, IS_MIXED, 0)
ZEND_END_ARG_INFO()

static const zend_function_entry functions[] = {
	ZEND_NS_FENTRY("Cairo", async, ZEND_FN(cairo_api_async), arginfo_async, 0)
	ZEND_NS_FENTRY("Cairo", await, ZEND_FN(cairo_api_await), arginfo_await, 0)
	ZEND_NS_FENTRY("Cairo", all, ZEND_FN(cairo_api_all), arginfo_all, 0)
	ZEND_NS_FENTRY("Cairo", sleep, ZEND_FN(cairo_api_sleep), arginfo_sleep, 0)
	ZEND_NS_FENTRY("Cairo", select, ZEND_FN(cairo_api_select), arginfo_select, 0)
	ZEND_NS_FENTRY("Cairo", offload, ZEND_FN(cairo_api_offload), arginfo_offload, 0)
	ZEND_FE_END
};

int cairo_api_startup(void)
{
	return zend_register_functions(NULL, functions, NULL, MODULE_PERSISTENT);
}

// putenv() and getenv() on an environment of the PHP request's own.
//
// PHP's putenv() writes the process's environment, and undoes it when the
// PHP request ends. The process is shared by all PHP threads, and libc's
// getenv(), which PHP itself calls at each request's start, takes no lock:
// a putenv() or the unsetenv() that undoes it, on one thread, while another
// reads, crashes the process. In classic mode every request is a PHP request,
// and Laravel's .env alone is 47 putenv() calls a request. So putenv() here
// changes only the PHP request's own table, which getenv() reads first: one
// request never sees what another set, and the process's environment is only
// read. What C libraries read from the environment (libpq, curl's proxies)
// does not see a putenv().

#include <php.h>
#include "env_hook.h"

static zif_handler original_getenv;

// The request's changes: name => string, or null for a name it unset. The
// request's memory: forgotten when the request ends (env_request_end()).
static __thread HashTable *changes;

static ZEND_NAMED_FUNCTION(cairo_putenv)
{
	char *setting;
	size_t len;
	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_STRING(setting, len)
	ZEND_PARSE_PARAMETERS_END();

	if (len == 0 || setting[0] == '=') {
		zend_argument_value_error(1, "must have a valid syntax");
		RETURN_THROWS();
	}
	if (changes == NULL) {
		ALLOC_HASHTABLE(changes);
		zend_hash_init(changes, 8, NULL, ZVAL_PTR_DTOR, 0);
	}
	const char *eq = memchr(setting, '=', len);
	zval value;
	if (eq == NULL) {
		ZVAL_NULL(&value);   // no '=': unset it, as PHP's putenv() does
		zend_hash_str_update(changes, setting, len, &value);
	} else {
		ZVAL_STRINGL(&value, eq + 1, len - (size_t)(eq - setting) - 1);
		zend_hash_str_update(changes, setting, (size_t)(eq - setting), &value);
	}
	RETURN_TRUE;
}

static ZEND_NAMED_FUNCTION(cairo_getenv)
{
	zend_string *name = NULL;
	bool local_only = false;
	ZEND_PARSE_PARAMETERS_START(0, 2)
		Z_PARAM_OPTIONAL
		Z_PARAM_STR_OR_NULL(name)
		Z_PARAM_BOOL(local_only)
	ZEND_PARSE_PARAMETERS_END();

	if (name != NULL && changes != NULL) {
		zval *v = zend_hash_find(changes, name);
		if (v != NULL) {
			if (Z_TYPE_P(v) == IS_NULL) {
				RETURN_FALSE;
			}
			RETURN_COPY(v);
		}
	}
	original_getenv(INTERNAL_FUNCTION_PARAM_PASSTHRU);
	if (name != NULL || changes == NULL || Z_TYPE_P(return_value) != IS_ARRAY) {
		return;
	}
	// getenv() with no name: the process's environment with the request's
	// changes over it.
	zend_string *key;
	zval *v;
	SEPARATE_ARRAY(return_value);
	ZEND_HASH_FOREACH_STR_KEY_VAL(changes, key, v) {
		if (key == NULL) {
			continue;
		}
		if (Z_TYPE_P(v) == IS_NULL) {
			zend_symtable_del(Z_ARRVAL_P(return_value), key);
		} else {
			Z_TRY_ADDREF_P(v);
			zend_symtable_update(Z_ARRVAL_P(return_value), key, v);
		}
	} ZEND_HASH_FOREACH_END();
}

// Swaps the handlers at module startup, before any PHP thread copies the
// function table.
static void env_startup(void)
{
	zend_function *put = zend_hash_str_find_ptr(CG(function_table), "putenv", sizeof("putenv") - 1);
	zend_function *get = zend_hash_str_find_ptr(CG(function_table), "getenv", sizeof("getenv") - 1);
	if (put == NULL || get == NULL || put->type != ZEND_INTERNAL_FUNCTION || get->type != ZEND_INTERNAL_FUNCTION) {
		return;
	}
	original_getenv = get->internal_function.handler;
	get->internal_function.handler = cairo_getenv;
	put->internal_function.handler = cairo_putenv;
}

static void env_request_end(void)
{
	changes = NULL;   // freed with the request's memory
}

const cairo_extension cairo_env_hook = {.startup = env_startup, .request_end = env_request_end};

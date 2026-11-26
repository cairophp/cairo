// putenv() and getenv() on an environment of the PHP request's own, and the
// environment of a classic script as PHP-FPM gives it.
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
//
// Under PHP-FPM a request's CGI variables (REQUEST_METHOD, HTTP_HOST, ...) are
// its environment too: getenv() finds them before anything putenv() set, and
// getenv() with no name and $_ENV list them. A classic script's are what
// $_SERVER holds as it starts (cairo_cgi_env()), and a script that changes
// $_SERVER later does not change them, as under PHP-FPM.

#include <php.h>
#include <php_variables.h>
#include "env_hook.h"

static zif_handler original_getenv;
static void (*original_import)(zval *array);

// The request's changes: name => string, or null for a name it unset.
static __thread HashTable *changes;
// The classic script's CGI variables: its $_SERVER as it started, shared
// with it (a write to $_SERVER separates it).
static __thread HashTable *cgi;

// The CGI variable name, as PHP-FPM has it. $_SERVER holds what PHP adds
// besides (PHP_SELF, and REQUEST_TIME and argv, which are no strings).
static zval *cgi_find(zend_string *name)
{
	if (cgi == NULL || zend_string_equals_literal(name, "PHP_SELF")) {
		return NULL;
	}
	zval *v = zend_hash_find(cgi, name);
	return v != NULL && Z_TYPE_P(v) == IS_STRING ? v : NULL;
}

// Lays the CGI variables over array, as PHP-FPM does for $_ENV and getenv().
static void cgi_over(HashTable *array)
{
	zend_string *key;
	zval *v;
	ZEND_HASH_FOREACH_STR_KEY(cgi, key) {
		if (key != NULL && (v = cgi_find(key)) != NULL) {
			Z_ADDREF_P(v);
			zend_symtable_update(array, key, v);
		}
	} ZEND_HASH_FOREACH_END();
}

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

// getenv() with no name: the process's environment, the request's changes
// over it, and the CGI variables over both.
static void getenv_all(zval *return_value)
{
	zend_string *key;
	zval *v;
	if (changes == NULL && cgi == NULL) {
		return;
	}
	SEPARATE_ARRAY(return_value);
	if (changes != NULL) {
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
	if (cgi != NULL) {
		cgi_over(Z_ARRVAL_P(return_value));
	}
}

// getenv($name): a CGI variable first (as PHP-FPM's SAPI answers before the
// process), unless $local_only, then the request's changes, then the process.
static ZEND_NAMED_FUNCTION(cairo_getenv)
{
	zend_string *name = NULL;
	bool local_only = false;
	ZEND_PARSE_PARAMETERS_START(0, 2)
		Z_PARAM_OPTIONAL
		Z_PARAM_STR_OR_NULL(name)
		Z_PARAM_BOOL(local_only)
	ZEND_PARSE_PARAMETERS_END();

	if (name != NULL) {
		zval *v = local_only ? NULL : cgi_find(name);
		if (v == NULL && changes != NULL) {
			v = zend_hash_find(changes, name);
		}
		if (v != NULL) {
			if (Z_TYPE_P(v) == IS_NULL) {
				RETURN_FALSE;
			}
			RETURN_COPY(v);
		}
	}
	original_getenv(INTERNAL_FUNCTION_PARAM_PASSTHRU);
	if (name == NULL && Z_TYPE_P(return_value) == IS_ARRAY) {
		getenv_all(return_value);
	}
}

// What PHP fills $_ENV from: the process's environment, with a classic
// script's CGI variables over it.
static void cairo_import_env(zval *array)
{
	original_import(array);
	if (cgi != NULL) {
		cgi_over(Z_ARRVAL_P(array));
	}
}

// cairo_cgi_env(array $server): void
// The CGI variables of the classic script about to run: its $_SERVER.
PHP_FUNCTION(cairo_cgi_env)
{
	HashTable *server;
	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_ARRAY_HT(server)
	ZEND_PARSE_PARAMETERS_END();
	if (cgi != NULL) {
		zend_array_release(cgi);
	}
	GC_TRY_ADDREF(server);
	cgi = server;
	// $_ENV that this request already made has them too.
	zval *env = &PG(http_globals)[TRACK_VARS_ENV];
	if (Z_TYPE_P(env) == IS_ARRAY) {
		SEPARATE_ARRAY(env);
		cgi_over(Z_ARRVAL_P(env));
	}
}

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_cgi_env, 0, 0, 1)
	ZEND_ARG_INFO(0, server)
ZEND_END_ARG_INFO()

static const zend_function_entry env_functions[] = {
	PHP_FE(cairo_cgi_env, arginfo_cairo_cgi_env)
	PHP_FE_END
};

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
	original_import = php_import_environment_variables;
	php_import_environment_variables = cairo_import_env;
}

static void env_request_shutdown(void)
{
	if (changes != NULL) {
		zend_hash_destroy(changes);
		FREE_HASHTABLE(changes);
		changes = NULL;
	}
	if (cgi != NULL) {
		zend_array_release(cgi);
		cgi = NULL;
	}
}

const cairo_extension cairo_env_hook = {
	.startup = env_startup,
	.request_shutdown = env_request_shutdown,
	.functions = env_functions,
};

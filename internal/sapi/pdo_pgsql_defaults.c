// PHP's pdo_pgsql as Cairo serves it.
//
// One round trip a query. pdo_pgsql prepares a statement on the server
// (PQprepare), runs it (PQexecPrepared) and deallocates it, three round trips
// for an app that prepares each statement it runs, as frameworks do. Each
// round trip is a wait of the fiber, so Cairo makes PDO::PGSQL_ATTR_DISABLE_PREPARES
// the default of a new connection: a statement then runs in one round trip
// (PQexecParams), with its parameters still sent apart from the SQL. A
// connection made with the attribute in its options keeps the app's choice,
// and setAttribute() changes it as usual.
//
// getNotify() with a timeout. pdo_pgsql waits for a notification in PHP's
// own poll() on the connection's socket, which libpq does not make: the
// wait is the fiber's here (libpq_wait.c takes that poll() while the fiber
// that called getNotify() runs it).
//
// Both are done by wrapping PHP's own functions at startup, after every
// extension has started: PDO's constructors, Pdo\Pgsql::getNotify() and the
// driver methods of a pgsql connection (PDO::pgsqlGetNotify()).

#include <pthread.h>
#include <string.h>

#include <php.h>
#include <ext/pdo/php_pdo_driver.h>

#include "libpq_wait.h"

// pdo_pgsql's PDO_PGSQL_ATTR_DISABLE_PREPARES, the first attribute of a driver
// since PHP 5.6; its header is not installed.
#define CAIRO_PGSQL_DISABLE_PREPARES PDO_ATTR_DRIVER_SPECIFIC

static zif_handler orig_construct, orig_connect, orig_get_notify, orig_ext_get_notify;
static zend_result (*prev_post_startup)(void);

// The fiber in getNotify(), whose poll() libpq_wait.c takes.
static __thread void *notify_fiber;

int cairo_pgsql_notify_wait(void)
{
	return notify_fiber != NULL && notify_fiber == (void *)EG(active_fiber);
}

static void notify_request_end(void)
{
	notify_fiber = NULL;
}

static void get_notify(INTERNAL_FUNCTION_PARAMETERS, zif_handler orig)
{
	void *outer = notify_fiber;
	notify_fiber = EG(active_fiber);
	orig(INTERNAL_FUNCTION_PARAM_PASSTHRU);
	notify_fiber = outer;
}

static ZEND_NAMED_FUNCTION(cairo_get_notify)
{
	get_notify(INTERNAL_FUNCTION_PARAM_PASSTHRU, orig_get_notify);
}

static ZEND_NAMED_FUNCTION(cairo_ext_get_notify)
{
	get_notify(INTERNAL_FUNCTION_PARAM_PASSTHRU, orig_ext_get_notify);
}

// A pgsql connection's methods: pdo_pgsql's, but with the driver methods
// (PDO::pgsqlGetNotify() and its siblings) whose getNotify waits on the
// fiber. Made from the first connection, the same for all.
static struct pdo_dbh_methods pgsql_methods;
static const struct pdo_dbh_methods *pgsql_orig_methods;
static zend_function_entry *pgsql_ext_methods;
static pthread_mutex_t methods_lock = PTHREAD_MUTEX_INITIALIZER;

static const zend_function_entry *driver_methods(pdo_dbh_t *dbh, int kind)
{
	return kind == PDO_DBH_DRIVER_METHOD_KIND_DBH && pgsql_ext_methods != NULL
		? pgsql_ext_methods
		: pgsql_orig_methods->get_driver_methods(dbh, kind);
}

// Copies pdo_pgsql's driver methods once, with the wrapped getNotify.
static void copy_driver_methods(pdo_dbh_t *dbh)
{
	const zend_function_entry *orig = dbh->methods->get_driver_methods != NULL
		? dbh->methods->get_driver_methods(dbh, PDO_DBH_DRIVER_METHOD_KIND_DBH)
		: NULL;
	size_t n = 0;
	while (orig != NULL && orig[n].fname != NULL) {
		n++;
	}
	if (n > 0) {
		zend_function_entry *copy = pemalloc(sizeof(*copy) * (n + 1), 1);
		memcpy(copy, orig, sizeof(*copy) * (n + 1));
		for (size_t i = 0; i < n; i++) {
			if (strcmp(copy[i].fname, "pgsqlGetNotify") == 0) {
				orig_ext_get_notify = copy[i].handler;
				copy[i].handler = cairo_ext_get_notify;
			}
		}
		pgsql_ext_methods = copy;
	}
	pgsql_methods = *dbh->methods;
	pgsql_methods.get_driver_methods = driver_methods;
	pgsql_orig_methods = dbh->methods;
}

// What Cairo makes of a connection that a PDO constructor just made.
static void adopt(zend_object *obj, zval *options)
{
	pdo_dbh_t *dbh = php_pdo_dbh_fetch_inner(obj);
	if (dbh == NULL || dbh->driver == NULL || dbh->driver_data == NULL || dbh->methods == NULL
		|| strcmp(dbh->driver->driver_name, "pgsql") != 0) {
		return;
	}
	if (options == NULL || Z_TYPE_P(options) != IS_ARRAY
		|| !zend_hash_index_exists(Z_ARRVAL_P(options), CAIRO_PGSQL_DISABLE_PREPARES)) {
		zval on;
		ZVAL_TRUE(&on);
		dbh->methods->set_attribute(dbh, CAIRO_PGSQL_DISABLE_PREPARES, &on);
	}
	pthread_mutex_lock(&methods_lock);
	if (pgsql_orig_methods == NULL) {
		copy_driver_methods(dbh);
	}
	pthread_mutex_unlock(&methods_lock);
	if (dbh->methods == pgsql_orig_methods) {
		dbh->methods = &pgsql_methods;
	}
}

static zval *options_arg(zend_execute_data *execute_data)
{
	return ZEND_NUM_ARGS() >= 4 ? ZEND_CALL_ARG(execute_data, 4) : NULL;
}

static ZEND_NAMED_FUNCTION(cairo_pdo_construct)
{
	orig_construct(INTERNAL_FUNCTION_PARAM_PASSTHRU);
	if (EG(exception) == NULL) {
		adopt(Z_OBJ_P(ZEND_THIS), options_arg(execute_data));
	}
}

static ZEND_NAMED_FUNCTION(cairo_pdo_connect)
{
	orig_connect(INTERNAL_FUNCTION_PARAM_PASSTHRU);
	if (EG(exception) == NULL && Z_TYPE_P(return_value) == IS_OBJECT) {
		adopt(Z_OBJ_P(return_value), options_arg(execute_data));
	}
}

// Points ce's method name at handler, when it is still orig's (a subclass
// shares or copies its parent's). Returns the handler it replaced.
static zif_handler wrap(zend_class_entry *ce, const char *name, size_t len, zif_handler orig, zif_handler handler)
{
	zend_function *fn = zend_hash_str_find_ptr(&ce->function_table, name, len);
	if (fn == NULL || fn->type != ZEND_INTERNAL_FUNCTION) {
		return NULL;
	}
	if (orig == NULL || fn->internal_function.handler == orig) {
		zif_handler was = fn->internal_function.handler;
		fn->internal_function.handler = handler;
		return was;
	}
	return NULL;
}

static zend_result wrap_pdo(void)
{
	if (prev_post_startup != NULL && prev_post_startup() != SUCCESS) {
		return FAILURE;
	}
	zend_class_entry *pdo = zend_hash_str_find_ptr(CG(class_table), ZEND_STRL("pdo"));
	if (pdo == NULL) {
		return SUCCESS;
	}
	orig_construct = wrap(pdo, ZEND_STRL("__construct"), NULL, cairo_pdo_construct);
	orig_connect = wrap(pdo, ZEND_STRL("connect"), NULL, cairo_pdo_connect);   // PHP 8.4 and later
	zend_class_entry *ce;
	ZEND_HASH_MAP_FOREACH_PTR(CG(class_table), ce) {
		if (ce != pdo && ce->type == ZEND_INTERNAL_CLASS && instanceof_function(ce, pdo)) {
			wrap(ce, ZEND_STRL("__construct"), orig_construct, cairo_pdo_construct);
			wrap(ce, ZEND_STRL("connect"), orig_connect, cairo_pdo_connect);
		}
	} ZEND_HASH_FOREACH_END();
	zend_class_entry *pg = zend_hash_str_find_ptr(CG(class_table), ZEND_STRL("pdo\\pgsql"));
	if (pg != NULL) {
		orig_get_notify = wrap(pg, ZEND_STRL("getnotify"), NULL, cairo_get_notify);
	}
	return SUCCESS;
}

static void pdo_pgsql_startup(void)
{
	prev_post_startup = zend_post_startup_cb;
	zend_post_startup_cb = wrap_pdo;
}

const cairo_extension cairo_pdo_pgsql = {.startup = pdo_pgsql_startup, .request_end = notify_request_end};

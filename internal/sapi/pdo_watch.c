// PDO methods that run a statement, wrapped so that the repeated query
// detector sees every statement whatever the driver (MySQL, Cairo's
// PostgreSQL, SQLite). Off, the wrappers cost one branch per statement.

#include <php.h>
#include <ext/pdo/php_pdo.h>
#include <ext/pdo/php_pdo_driver.h>

#include "pdo_watch.h"

// Per thread, as each PHP thread turns it on for itself.
static __thread bool cairo_watching = false;

static zif_handler cairo_orig_query = NULL;
static zif_handler cairo_orig_exec = NULL;
static zif_handler cairo_orig_execute = NULL;

// cairo_watch_queries(bool $on): void
PHP_FUNCTION(cairo_watch_queries)
{
	bool on;
	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_BOOL(on)
	ZEND_PARSE_PARAMETERS_END();
	cairo_watching = on;
}

static void cairo_report_query(zend_string *sql)
{
	if (sql == NULL || EG(exception) != NULL) {
		return;
	}
	zval fn, ret, arg;
	ZVAL_STRING(&fn, "Cairo\\Internal\\Queries::seen");
	ZVAL_STR_COPY(&arg, sql);
	ZVAL_UNDEF(&ret);
	call_user_function(NULL, NULL, &fn, &ret, 1, &arg);
	zval_ptr_dtor(&fn);
	zval_ptr_dtor(&arg);
	zval_ptr_dtor(&ret);
}

// The statement of PDO::query($sql, ...) and PDO::exec($sql).
static void cairo_report_first_arg(zend_execute_data *execute_data)
{
	if (ZEND_NUM_ARGS() >= 1 && Z_TYPE_P(ZEND_CALL_ARG(execute_data, 1)) == IS_STRING) {
		cairo_report_query(Z_STR_P(ZEND_CALL_ARG(execute_data, 1)));
	}
}

static ZEND_NAMED_FUNCTION(cairo_pdo_query)
{
	if (cairo_watching) {
		cairo_report_first_arg(execute_data);
		if (EG(exception) != NULL) {
			return;
		}
	}
	cairo_orig_query(INTERNAL_FUNCTION_PARAM_PASSTHRU);
}

static ZEND_NAMED_FUNCTION(cairo_pdo_exec)
{
	if (cairo_watching) {
		cairo_report_first_arg(execute_data);
		if (EG(exception) != NULL) {
			return;
		}
	}
	cairo_orig_exec(INTERNAL_FUNCTION_PARAM_PASSTHRU);
}

static ZEND_NAMED_FUNCTION(cairo_pdo_execute)
{
	if (cairo_watching && Z_TYPE(EX(This)) == IS_OBJECT) {
		cairo_report_query(Z_PDO_STMT_P(&EX(This))->query_string);
		if (EG(exception) != NULL) {
			return;
		}
	}
	cairo_orig_execute(INTERNAL_FUNCTION_PARAM_PASSTHRU);
}

// Swaps orig for wrapper in method name of every internal class, not just
// PDO's own: the driver subclasses (Pdo\Mysql, Pdo\Sqlite) copied the
// method before this module started.
static void cairo_wrap_method(const char *name, size_t len, zif_handler orig, zif_handler wrapper)
{
	zend_class_entry *ce;
	ZEND_HASH_MAP_FOREACH_PTR(CG(class_table), ce) {
		if (ce->type != ZEND_INTERNAL_CLASS) {
			continue;
		}
		zend_function *fn = zend_hash_str_find_ptr(&ce->function_table, name, len);
		if (fn != NULL && fn->type == ZEND_INTERNAL_FUNCTION && fn->internal_function.handler == orig) {
			fn->internal_function.handler = wrapper;
		}
	} ZEND_HASH_FOREACH_END();
}

static zif_handler cairo_find_method(const char *class_lc, size_t class_len, const char *name, size_t len)
{
	zend_class_entry *ce = zend_hash_str_find_ptr(CG(class_table), class_lc, class_len);
	if (ce == NULL) {
		return NULL;
	}
	zend_function *fn = zend_hash_str_find_ptr(&ce->function_table, name, len);
	return fn != NULL && fn->type == ZEND_INTERNAL_FUNCTION ? fn->internal_function.handler : NULL;
}

void cairo_watch_pdo(void)
{
	cairo_orig_query = cairo_find_method("pdo", 3, "query", 5);
	cairo_orig_exec = cairo_find_method("pdo", 3, "exec", 4);
	cairo_orig_execute = cairo_find_method("pdostatement", 12, "execute", 7);
	if (cairo_orig_query == NULL || cairo_orig_exec == NULL || cairo_orig_execute == NULL) {
		return;   // no PDO in this build
	}
	cairo_wrap_method("query", 5, cairo_orig_query, cairo_pdo_query);
	cairo_wrap_method("exec", 4, cairo_orig_exec, cairo_pdo_exec);
	cairo_wrap_method("execute", 7, cairo_orig_execute, cairo_pdo_execute);
}

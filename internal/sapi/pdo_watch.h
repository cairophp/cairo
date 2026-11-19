#ifndef CAIRO_PDO_WATCH_H
#define CAIRO_PDO_WATCH_H

#include <php.h>

// The repeated query detector (App::repeatedQueries()): PDO::query(),
// PDO::exec() and PDOStatement::execute() tell Cairo\Internal\Queries the
// statement they run, on threads that turned it on. Included once, by
// cairo.c, for its function table.

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_watch_queries, 0, 0, 1)
	ZEND_ARG_INFO(0, on)
ZEND_END_ARG_INFO()

PHP_FUNCTION(cairo_watch_queries);

// Wraps the PDO methods, at module startup.
void cairo_watch_pdo(void);

#endif

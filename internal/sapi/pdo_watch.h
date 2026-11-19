#ifndef CAIRO_PDO_WATCH_H
#define CAIRO_PDO_WATCH_H

#include "extension.h"

// The repeated query detector (App::repeatedQueries()): PDO::query(),
// PDO::exec() and PDOStatement::execute() tell Cairo\Internal\Queries the
// statement they run, on threads that turned it on. repeated.go adds it to
// the SAPI module.
extern const cairo_extension cairo_pdo_watch;

#endif

package sapi

/*
#include "libpq_wait.h"
*/
import "C"

// PDO pgsql is PHP's own pdo_pgsql, on libpq: its waits for the server park
// the fiber (libpq_wait.c), and it runs a query in one round trip by default
// (pdo_pgsql_defaults.c). All of it is C; this file adds it to the SAPI
// module.
func init() {
	registerC(&C.cairo_pdo_pgsql)
}

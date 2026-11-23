// What the SAPI in cairo.c shares with the command line in cli.c.
#ifndef CAIRO_MODULE_H
#define CAIRO_MODULE_H

#include <php.h>

// The cairo module: Cairo's PHP functions and the startup of the extensions.
extern zend_module_entry cairo_module_entry;

void cairo_signals_onstack(void);

// The calling thread is PHP thread tid from now on (cairo_thread_id()): the
// thread of the command line, which cairo_thread_run() did not start.
void cairo_thread_adopt(int tid);

// cairo_cli_script(): void, in cli.c, for the module's function table.
PHP_FUNCTION(cairo_cli_script);

#endif

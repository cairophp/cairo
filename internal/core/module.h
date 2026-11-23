// What the SAPI in cairo.c shares with the command line in cli.c.
#ifndef CAIRO_MODULE_H
#define CAIRO_MODULE_H

#include <php.h>

// The cairo module: Cairo's PHP functions and the startup of the extensions.
extern zend_module_entry cairo_module_entry;

void cairo_signals_onstack(void);

#endif

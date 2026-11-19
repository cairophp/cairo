#ifndef CAIRO_HOOKS_H
#define CAIRO_HOOKS_H

#include "extension.h"

// The runtime hooks; hookops.go adds them to the SAPI module.
extern const cairo_extension cairo_function_hooks;   // function_hook.c
extern const cairo_extension cairo_socket_hooks;     // socket_hook.c

#endif

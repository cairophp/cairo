#ifndef CAIRO_ENV_HOOK_H
#define CAIRO_ENV_HOOK_H

#include "extension.h"

// putenv() and getenv() on an environment of the PHP request's own; env.go
// adds it to the SAPI module.
extern const cairo_extension cairo_env_hook;

// The CGI variables of the script about to run, in C: fill registers them
// into an array on their first use, as the SAPI registers $_SERVER's.
void cairo_cgi_env_from(void (*fill)(struct _zval_struct *array));

#endif

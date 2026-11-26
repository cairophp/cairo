#ifndef CAIRO_ENV_HOOK_H
#define CAIRO_ENV_HOOK_H

#include "extension.h"

// putenv() and getenv() on an environment of the PHP request's own; env.go
// adds it to the SAPI module.
extern const cairo_extension cairo_env_hook;

#endif

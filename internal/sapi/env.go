package sapi

/*
#include "env_hook.h"
*/
import "C"

// putenv() and getenv() on the PHP request's own environment are C
// (env_hook.c); env.php makes it each request's own in fiber and isolated
// mode.
func init() {
	registerC(&C.cairo_env_hook)
	registerPHP("env.php")
}

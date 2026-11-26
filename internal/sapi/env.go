package sapi

/*
#include "env_hook.h"
*/
import "C"

// putenv() and getenv() on the PHP request's own environment are all C
// (env_hook.c): this file only adds them to the SAPI module.
func init() {
	registerC(&C.cairo_env_hook)
}

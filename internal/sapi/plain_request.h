#ifndef CAIRO_PLAIN_REQUEST_H
#define CAIRO_PLAIN_REQUEST_H

#include "extension.h"

// The scripts of cairo serve with no declaration, served in C as PHP's own
// SAPI requests (plain_request.c); scripts.go adds it to the SAPI module.
extern const cairo_extension cairo_plain_request;

#endif

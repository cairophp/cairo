#ifndef CAIRO_BLOCKING_H
#define CAIRO_BLOCKING_H

#include "extension.h"

// The report of App::detectBlocking(); blocking.go adds it to the SAPI
// module.
extern const cairo_extension cairo_blocking;

// Called from Go's blocking detector, which then interrupts the thread:
// thread tid has run PHP for ms milliseconds without waiting for an event.
// At its next safe point the running fiber logs where it is.
void cairo_report_blocking(int tid, int ms);

#endif

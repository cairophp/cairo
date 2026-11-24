#ifndef CAIRO_SIGNALS_H
#define CAIRO_SIGNALS_H

#include "extension.h"

// Signals for the scripts of `cairo php` (signals.c); signals.go adds it to
// the SAPI module.
extern const cairo_extension cairo_signals;

// Called by Go for a signal that os/signal gave it: brings it to the
// script's thread. Returns 0 when the script no longer handles the signal.
int cairo_signal_deliver(int signo);

// Interrupts the script's thread again while a signal still waits for one
// of its fibers. Returns 0 when none does.
int cairo_signals_interrupt_again(void);

#endif

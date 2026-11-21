#ifndef CAIRO_STACKBOUND_H
#define CAIRO_STACKBOUND_H

#include <stdint.h>

// What Go's runtime asks on a call from C: the bounds of the calling thread's
// stack. Returns how many of this thread's questions went on to glibc (0 with
// another libc, where none is held back), or -1 when the question failed.
int cairo_stackbound_probe(uintptr_t bounds[2]);

#endif

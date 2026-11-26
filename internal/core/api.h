#ifndef CAIRO_API_H
#define CAIRO_API_H

// Cairo's concurrency functions (async(), await(), all(), sleep(), select(),
// offload()) as C functions of the module: every PHP request has them at no
// cost, and each runs its PHP in Cairo\Internal\Fn.
int cairo_api_startup(void);

#endif

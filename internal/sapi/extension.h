#ifndef CAIRO_EXTENSION_H
#define CAIRO_EXTENSION_H

// The C side of an extension: what the SAPI module (cairo.c) does on its
// behalf, so that the module names no extension. An extension defines one
// and its Go file adds it from init() with registerC. Every member may be
// NULL.
struct _zend_function_entry;

typedef struct cairo_extension {
	void (*startup)(void);      // module startup: once, before any PHP thread exists
	void (*thread_end)(void);   // on a PHP thread that ends, after its last request
	const struct _zend_function_entry *functions;   // PHP functions it adds
} cairo_extension;

// Adds an extension; returns -1 when the table is full. Extensions start in
// the order they were added and must not rely on one another. Only before
// the engine boots: nothing guards the table.
int cairo_extension_add(const cairo_extension *ext);

// For the SAPI module.
int cairo_extensions_startup(void);   // SUCCESS or FAILURE
void cairo_extensions_thread_end(void);

#endif

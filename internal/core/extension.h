#ifndef CAIRO_EXTENSION_H
#define CAIRO_EXTENSION_H

// The C side of an extension: what the SAPI module (cairo.c) does on its
// behalf, so that the module names no extension. An extension defines one
// and its Go file adds it from init() with RegisterC. Every member may be
// NULL.
struct _zend_function_entry;

typedef struct cairo_extension {
	void (*startup)(void);      // module startup: once, before any PHP thread exists
	void (*thread_end)(void);   // on a PHP thread that ends, after its last request
	void (*interrupt)(int tid); // on PHP thread tid, once the engine interrupted it: see below
	const struct _zend_function_entry *functions;   // PHP functions it adds
} cairo_extension;

// Adds an extension; returns -1 when the table is full. Extensions start in
// the order they were added and must not rely on one another. Only before
// the engine boots: nothing guards the table.
int cairo_extension_add(const cairo_extension *ext);

// How an extension gets the attention of a request thread that is running
// PHP: its Go code calls Engine.InterruptThread() with the thread's id, and
// at its next safe point (a loop back-edge or a call) that thread runs the
// interrupt of every extension, inside whatever fiber is running and with no
// exception pending. An extension tells its own interrupts from the others'
// by what it kept for the thread: tid is 0 or more and below
// CAIRO_MAX_THREADS.
#define CAIRO_MAX_THREADS 4096

// The id of the calling PHP thread, or -1 on a thread that runs no requests
// or offload jobs (the declare phase). The engine looks up what a thread
// asks for in the thread's own tables, so C passes this with every such
// call: an extension with what it answers (Respond in dispatch.go).
int cairo_thread_id(void);

// For the SAPI module.
int cairo_extensions_startup(void);   // SUCCESS or FAILURE
void cairo_extensions_thread_end(void);
void cairo_extensions_interrupt(int tid);

#endif

#ifndef CAIRO_EXTENSION_H
#define CAIRO_EXTENSION_H

#include <stddef.h>
#include <stdint.h>

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

// What Engine.InterruptThread() does, for C code: thread tid runs the
// interrupts at its next safe point. From any thread; a thread that runs no
// PHP is left alone.
void cairo_vm_interrupt(int tid);

// The id of the calling PHP thread, or -1 on a thread that runs no requests
// or offload jobs (the declare phase). The engine looks up what a thread
// asks for in the thread's own tables, so C passes this with every such
// call.
int cairo_thread_id(void);

// Answers call id, which the calling PHP thread took: meta is what the
// call's server makes of the answer (n bytes), body an HTTP response's body
// (bn bytes), neither longer than INT_MAX. Both are copied, and Go gets
// them without this thread running Go code (outbox.h). Returns -1 when
// one is longer or there is no memory for the copy.
int cairo_answer(int64_t id, const char *meta, size_t n, const char *body, size_t bn);

// For the SAPI module.
int cairo_extensions_startup(void);   // SUCCESS or FAILURE
void cairo_extensions_thread_end(void);
void cairo_extensions_interrupt(int tid);

#endif

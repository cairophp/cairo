// A request thread whose requests an extension serves in C (cserve.h).

#include <php.h>
#include <SAPI.h>

#include "cserve.h"
#include "loop.h"
#include "queue.h"

static __thread bool on;          // outlives the PHP requests of the thread
static __thread bool for_runtime; // the next PHP request is for an event the runtime takes
static __thread cairo_msg *call;  // of the PHP request that the server serves
static __thread const cairo_cserver *server;

// SAPI.c reads the body and the cookies only for a request that has one.
static char server_context;

// cairo_cserve(): bool
// The calling request thread's next PHP requests are served in C, by the
// extension that has a cairo_cserver. False when none has, or when this PHP
// request runs for an event that is not a call, which the runtime then takes.
PHP_FUNCTION(cairo_cserve)
{
	ZEND_PARSE_PARAMETERS_NONE();
	if (for_runtime || cairo_extensions_cserver() == NULL || cairo_thread_id() < 0) {
		RETURN_FALSE;
	}
	on = true;
	RETURN_TRUE;
}

bool cairo_cserve_take(int tid)
{
	for_runtime = false;
	if (!on) {
		return false;
	}
	call = cairo_loop_take_call(tid);
	if (call == NULL) {
		for_runtime = true;
		return false;
	}
	server = cairo_extensions_cserver();
	SG(server_context) = &server_context;
	server->begin(call->id, call->data, call->len);
	return true;
}

const cairo_cserver *cairo_cserving(void)
{
	return call != NULL ? server : NULL;
}

void cairo_cserve_done(void)
{
	if (call == NULL) {
		return;
	}
	server->end();
	free(call);
	call = NULL;
	server = NULL;
	SG(server_context) = NULL;
}

void cairo_cserve_forget(void)
{
	on = for_runtime = false;
}

// What a fiber asks Go for (see waitops.go, and submit in operation.go):
// cairo_fiber_call() takes the request the fiber belongs to and the scope of
// its context (Context::goScope()), and returns the operation's id, or -1
// when Go refused it; the fiber then suspends until the operation's result
// comes as an event.

#include <php.h>

#include "extension.h"
#include "loop.h"
#include "wire.h"
#include "_cgo_export.h"

// cairo_fiber_call(int $request, float $scope, string $op, string|array $payload): int
// An array is the arguments of a Go operation, which cross in internal/wire's
// form (wire.Unmarshal reads them); a string crosses as it is.
PHP_FUNCTION(cairo_fiber_call)
{
	zend_long request;
	double scope;
	zend_string *name, *payload;
	HashTable *args;
	ZEND_PARSE_PARAMETERS_START(4, 4)
		Z_PARAM_LONG(request)
		Z_PARAM_DOUBLE(scope)
		Z_PARAM_STR(name)
		Z_PARAM_ARRAY_HT_OR_STR(args, payload)
	ZEND_PARSE_PARAMETERS_END();
	smart_str encoded = {0};
	if (args != NULL) {
		if (cairo_wire_encode(args, &encoded) == FAILURE) {
			smart_str_free(&encoded);
			RETURN_THROWS();
		}
		payload = encoded.s;
	}
	zend_long id = -1;
	if (ZSTR_LEN(name) <= INT_MAX && ZSTR_LEN(payload) <= INT_MAX) {
		id = (zend_long)goFiberCall(cairo_thread_id(), (long long)request, scope, ZSTR_VAL(name), (int)ZSTR_LEN(name),
			ZSTR_VAL(payload), (int)ZSTR_LEN(payload));
	}
	smart_str_free(&encoded);
	RETURN_LONG(id);
}

// cairo_op_cancel(int $id): void
// Ends the context of an operation that this thread submitted with a scope,
// when PHP cancels a context it runs under. The operation still reports,
// with the context's error.
PHP_FUNCTION(cairo_op_cancel)
{
	zend_long id;
	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_LONG(id)
	ZEND_PARSE_PARAMETERS_END();
	goOpCancel(cairo_thread_id(), (long long)id);
}

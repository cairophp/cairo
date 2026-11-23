// The fibers of a PHP thread, as its scheduler keeps them. See fibers.h.

#include <php.h>
#include <zend_exceptions.h>
#include <zend_extensions.h>
#include <zend_fibers.h>
#include <zend_observer.h>

#include "fiber_local.h"
#include "fibers.h"
#include "loop.h"

// How many fibers wait for their next job: a new one costs a stack mmap
// and, when it is freed, a TLB shootdown on every CPU.
#define CAIRO_IDLE_FIBERS 64

// Where a fiber's context is, among the reserved pointers of its
// zend_fiber_context: a reference to the Cairo\Context object, or NULL.
static int slot = -1;

// All per thread, in the memory of its PHP request, which ends with the
// thread. Each list holds a reference to its fibers.
static __thread zend_fiber *idle[CAIRO_IDLE_FIBERS];
static __thread uint32_t nidle;
static __thread zend_fiber **ready;  // a ring: the next to resume is at head
static __thread uint32_t ready_head, ready_len, ready_cap;
static __thread zend_object **orphans;  // contexts of destroyed fibers, until release_orphans()
static __thread uint32_t norphans, orphans_cap;

static void fiber_error(const char *message)
{
	zend_string *name = zend_string_init(ZEND_STRL("FiberError"), 0);
	zend_class_entry *ce = zend_lookup_class(name);
	zend_string_release(name);
	zend_throw_error(ce, "%s", message);
}

// Whether the code that runs may switch fibers; throws when it may not.
static bool may_switch(void)
{
	if (UNEXPECTED(zend_fiber_switch_blocked())) {
		fiber_error("Cannot switch fibers in current execution context");
		return false;
	}
	return true;
}

static bool suspended(const zend_fiber *fiber)
{
	return fiber->context.status == ZEND_FIBER_STATUS_SUSPENDED && fiber->caller == NULL;
}

#if PHP_VERSION_ID < 80400
// PHP below 8.4 has no zend_fiber_resume() and zend_fiber_suspend() to
// call. The three functions below are theirs, adapted from
// Zend/zend_fibers.c of PHP 8.3 (third_party/NOTICES).
static zend_fiber_transfer transfer_to(zend_fiber_context *to, zval *value)
{
	zend_fiber_transfer transfer = {.context = to, .flags = 0};
	if (value != NULL) {
		ZVAL_COPY(&transfer.value, value);
	} else {
		ZVAL_NULL(&transfer.value);
	}
	zend_fiber_switch_context(&transfer);
	if (UNEXPECTED(transfer.flags & ZEND_FIBER_TRANSFER_FLAG_BAILOUT)) {
		EG(active_fiber) = NULL;
		zend_bailout();  // a fatal error over there is one here
	}
	return transfer;
}

static void transferred(zend_fiber_transfer *transfer, zval *ret)
{
	if (transfer->flags & ZEND_FIBER_TRANSFER_FLAG_ERROR) {
		// Not zend_throw_exception_object(): an unwind (exit) is no Throwable.
		zend_throw_exception_internal(Z_OBJ(transfer->value));
		return;
	}
	ZVAL_COPY_VALUE(ret, &transfer->value);
}
#endif

void cairo_fiber_resume(zend_fiber *fiber, zval *value, zval *ret)
{
	ZVAL_NULL(ret);
#if PHP_VERSION_ID >= 80400
	zend_fiber_resume(fiber, value, ret);
#else
	zend_fiber *previous = EG(active_fiber);
	fiber->stack_bottom->prev_execute_data = EG(current_execute_data);
	if (previous != NULL) {
		previous->execute_data = EG(current_execute_data);
	}
	fiber->caller = EG(current_fiber_context);
	EG(active_fiber) = fiber;
	zend_fiber_transfer transfer = transfer_to(fiber->previous, value);
	EG(active_fiber) = previous;
	transferred(&transfer, ret);
#endif
}

zend_result cairo_fiber_suspend(zval *ret)
{
	zend_fiber *fiber = EG(active_fiber);
	ZVAL_NULL(ret);
	if (UNEXPECTED(fiber == NULL)) {
		fiber_error("Cannot suspend outside of a fiber");
		return FAILURE;
	}
	if (UNEXPECTED(fiber->flags & ZEND_FIBER_FLAG_DESTROYED)) {
		fiber_error("Cannot suspend in a force-closed fiber");
		return FAILURE;
	}
	if (!may_switch()) {
		return FAILURE;
	}
#if PHP_VERSION_ID >= 80400
	zend_fiber_suspend(fiber, NULL, ret);
#else
	zend_fiber_context *caller = fiber->caller;
	fiber->execute_data = EG(current_execute_data);
	fiber->stack_bottom->prev_execute_data = NULL;
	fiber->previous = EG(current_fiber_context);
	fiber->caller = NULL;
	zend_fiber_transfer transfer = transfer_to(caller, NULL);
	transferred(&transfer, ret);
#endif
	return EG(exception) == NULL ? SUCCESS : FAILURE;
}

// A fiber's context is released only from PHP functions, never while a
// fiber is destroyed: what the context holds may have destructors, and PHP
// code must not run inside a fiber switch.
static void fiber_destroyed(zend_fiber_context *ctx)
{
	zend_object *context = ctx->reserved[slot];
	if (context == NULL) {
		return;
	}
	ctx->reserved[slot] = NULL;
	if (norphans == orphans_cap) {
		orphans_cap = orphans_cap == 0 ? 16 : orphans_cap * 2;
		orphans = safe_erealloc(orphans, orphans_cap, sizeof(*orphans), 0);
	}
	orphans[norphans++] = context;
}

static void release_orphans(void)
{
	while (norphans > 0) {
		OBJ_RELEASE(orphans[--norphans]);
	}
}

static void set_context(zend_fiber *fiber, zend_object *context)
{
	zend_object *old = fiber->context.reserved[slot];
	if (context != NULL) {
		GC_ADDREF(context);
	}
	fiber->context.reserved[slot] = context;
	if (old != NULL) {
		OBJ_RELEASE(old);
	}
}

// cairo_fiber_context(): ?object
// The context that the running fiber is bound to; null when it has none,
// or when no fiber runs.
PHP_FUNCTION(cairo_fiber_context)
{
	ZEND_PARSE_PARAMETERS_NONE();
	zend_fiber *fiber = EG(active_fiber);
	zend_object *context = fiber != NULL ? fiber->context.reserved[slot] : NULL;
	if (context == NULL) {
		RETURN_NULL();
	}
	RETURN_OBJ_COPY(context);
}

// cairo_fiber_bind(Fiber $fiber, ?object $context, int $request): void
// $fiber runs under $context from now on, for request $request (0: none),
// whose request-local statics it sees (fiber_local.h).
PHP_FUNCTION(cairo_fiber_bind)
{
	zval *fiber;
	zend_object *context;
	zend_long request;
	ZEND_PARSE_PARAMETERS_START(3, 3)
		Z_PARAM_OBJECT_OF_CLASS(fiber, zend_ce_fiber)
		Z_PARAM_OBJ_OR_NULL(context)
		Z_PARAM_LONG(request)
	ZEND_PARSE_PARAMETERS_END();
	release_orphans();
	set_context((zend_fiber *)Z_OBJ_P(fiber), context);
	cairo_local_bind_fiber((zend_fiber *)Z_OBJ_P(fiber), request);
}

// cairo_fiber_reuse(object $context, int $request, mixed $job): bool
// Hands $job to an idle fiber, bound to $context and request $request as
// by cairo_fiber_bind(), and returns once that fiber suspends or is idle
// again. False when no fiber is idle: the caller starts a new one.
PHP_FUNCTION(cairo_fiber_reuse)
{
	zend_object *context;
	zend_long request;
	zval *job;
	ZEND_PARSE_PARAMETERS_START(3, 3)
		Z_PARAM_OBJ(context)
		Z_PARAM_LONG(request)
		Z_PARAM_ZVAL(job)
	ZEND_PARSE_PARAMETERS_END();
	release_orphans();
	if (!may_switch()) {
		RETURN_THROWS();
	}
	while (nidle > 0) {
		zend_fiber *fiber = idle[--nidle];  // with the list's reference
		if (EXPECTED(suspended(fiber))) {
			zval ret;
			set_context(fiber, context);
			cairo_local_bind_fiber(fiber, request);
			cairo_fiber_resume(fiber, job, &ret);
			zval_ptr_dtor(&ret);
			OBJ_RELEASE(&fiber->std);
			RETURN_TRUE;
		}
		OBJ_RELEASE(&fiber->std);
	}
	RETURN_FALSE;
}

// cairo_fiber_idle(): mixed
// The running fiber has done its job: it lets go of its context and its
// request, and waits for the next job, which cairo_fiber_reuse() returns
// here. Null when enough fibers are idle already, or when PHP is destroying
// this one (its thread ends, and what it ran caught the exit): it ends.
PHP_FUNCTION(cairo_fiber_idle)
{
	ZEND_PARSE_PARAMETERS_NONE();
	zend_fiber *fiber = EG(active_fiber);
	if (fiber == NULL) {
		fiber_error("Cannot suspend outside of a fiber");
		RETURN_THROWS();
	}
	release_orphans();
	set_context(fiber, NULL);
	cairo_local_bind_fiber(fiber, 0);
	if (EG(exception) != NULL || nidle >= CAIRO_IDLE_FIBERS || (fiber->flags & ZEND_FIBER_FLAG_DESTROYED)) {
		return;
	}
	GC_ADDREF(&fiber->std);
	idle[nidle++] = fiber;
	if (cairo_fiber_suspend(return_value) == FAILURE) {
		RETURN_THROWS();
	}
}

uint32_t cairo_fibers_queued(void)
{
	return ready_len;
}

void cairo_fibers_queue(zend_fiber *fiber)
{
	if (ready_len == ready_cap) {
		uint32_t cap = ready_cap == 0 ? 64 : ready_cap * 2;
		zend_fiber **ring = safe_emalloc(cap, sizeof(*ring), 0);
		for (uint32_t i = 0; i < ready_len; i++) {
			ring[i] = ready[(ready_head + i) & (ready_cap - 1)];
		}
		if (ready != NULL) {
			efree(ready);
		}
		ready = ring;
		ready_head = 0;
		ready_cap = cap;
	}
	GC_ADDREF(&fiber->std);
	ready[(ready_head + ready_len++) & (ready_cap - 1)] = fiber;
}

// cairo_ready(Fiber $fiber): void
// $fiber, which is suspended, runs again at the loop's next turn.
PHP_FUNCTION(cairo_ready)
{
	zval *fiber;
	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_OBJECT_OF_CLASS(fiber, zend_ce_fiber)
	ZEND_PARSE_PARAMETERS_END();
	cairo_fibers_queue((zend_fiber *)Z_OBJ_P(fiber));
}

// cairo_run_ready(int $max): bool
// Resumes the fibers that are ready, in the order they became so, $max of
// them at most, and says whether none is left. A fiber that is not
// suspended any more (it ended, or something else resumed it) is skipped.
PHP_FUNCTION(cairo_run_ready)
{
	zend_long max;
	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_LONG(max)
	ZEND_PARSE_PARAMETERS_END();
	if (max > 0 && ready_len > 0 && !may_switch()) {
		RETURN_THROWS();
	}
	while (max-- > 0 && ready_len > 0) {
		zend_fiber *fiber = ready[ready_head];
		ready_head = (ready_head + 1) & (ready_cap - 1);
		ready_len--;
		if (EXPECTED(suspended(fiber))) {
			zval ret;
			cairo_loop_resumes();
			cairo_fiber_resume(fiber, NULL, &ret);
			zval_ptr_dtor(&ret);
		}
		OBJ_RELEASE(&fiber->std);
		if (UNEXPECTED(EG(exception) != NULL)) {
			RETURN_THROWS();
		}
	}
	RETURN_BOOL(ready_len == 0);
}

zend_result cairo_fibers_startup(void)
{
	slot = zend_get_resource_handle("cairo fibers");
	if (slot < 0) {
		return FAILURE;  // every reserved pointer of a fiber is taken
	}
	zend_observer_fiber_destroy_register(fiber_destroyed);
	return SUCCESS;
}

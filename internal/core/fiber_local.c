// Request-local statics. A registered static property is a slot: while a
// fiber of request R runs, the slot holds R's value, and the values of the
// other requests on the thread wait in their stores. At a fiber switch
// between two requests, each slot's value moves out to the store of the
// request that was running and the next request's value moves in: zval
// moves, no copies and no refcounting. A switch within one request, or
// any switch on a thread with no slot, costs one comparison.
//
// The boot store holds the values outside any request (the scheduler,
// server.php); a request starts with copies of them. A request's store is
// referenced by the request, until cairo_local_end(), and by each fiber
// bound to it, so it lives as long as any of its fibers.

#include <php.h>
#include <zend_fibers.h>
#include <zend_extensions.h>
#include <zend_observer.h>

#include "fiber_local.h"

typedef struct cairo_local_store {
	uint32_t refs;
	zval *values;  // one per slot; UNDEF while its request runs
	struct cairo_local_store *next_dead;
} cairo_local_store;

// All per thread, in the memory of its PHP request: each PHP request registers
// its slots when it runs server.php (cairo_local_request_end() forgets them).
static __thread zval **cairo_slots;  // the static properties
static __thread uint32_t cairo_slot_count;
static __thread HashTable cairo_requests;  // request id -> its store, until cairo_local_end()
static __thread bool cairo_requests_init;
static __thread cairo_local_store cairo_boot;
static __thread cairo_local_store *cairo_live;  // whose values are in the slots; NULL: the boot store
static __thread cairo_local_store *cairo_dead;  // unreferenced, freed at the next safe point

// A fiber's store, in a reserved slot of its context (NULL: the boot store).
static int cairo_local_slot = -1;

static cairo_local_store *cairo_running(void)
{
	return cairo_live != NULL ? cairo_live : &cairo_boot;
}

// Values are freed only from PHP functions, never inside a fiber switch,
// where a destructor running PHP code could switch fibers itself.
static void cairo_free_dead(void)
{
	while (cairo_dead != NULL) {
		cairo_local_store *s = cairo_dead;
		cairo_dead = s->next_dead;
		for (uint32_t i = 0; i < cairo_slot_count; i++) {
			zval_ptr_dtor(&s->values[i]);
		}
		efree(s->values);
		efree(s);
	}
}

static void cairo_release(cairo_local_store *s)
{
	if (s == NULL || --s->refs > 0) {
		return;
	}
	if (s == cairo_live) {
		return;  // its values are in the slots: queued when the switch moves them out
	}
	s->next_dead = cairo_dead;
	cairo_dead = s;
}

static cairo_local_store *cairo_new_store(void)
{
	cairo_local_store *s = emalloc(sizeof(*s));
	s->refs = 1;
	s->next_dead = NULL;
	s->values = safe_emalloc(cairo_slot_count, sizeof(zval), 0);
	for (uint32_t i = 0; i < cairo_slot_count; i++) {
		// The boot values are in the slots while no request runs.
		ZVAL_COPY(&s->values[i], cairo_live == NULL ? cairo_slots[i] : &cairo_boot.values[i]);
	}
	return s;
}

static void cairo_local_switch(zend_fiber_context *from, zend_fiber_context *to)
{
	(void)from;
	if (cairo_slot_count == 0) {
		return;
	}
	cairo_local_store *next = to->reserved[cairo_local_slot];
	if (next == cairo_live) {
		return;
	}
	cairo_local_store *out = cairo_running();
	cairo_local_store *in = next != NULL ? next : &cairo_boot;
	for (uint32_t i = 0; i < cairo_slot_count; i++) {
		ZVAL_COPY_VALUE(&out->values[i], cairo_slots[i]);
		ZVAL_COPY_VALUE(cairo_slots[i], &in->values[i]);
		ZVAL_UNDEF(&in->values[i]);
	}
	cairo_live = next;
	if (out != &cairo_boot && out->refs == 0) {
		out->next_dead = cairo_dead;
		cairo_dead = out;
	}
}

static void cairo_local_destroy(zend_fiber_context *ctx)
{
	cairo_release(ctx->reserved[cairo_local_slot]);
	ctx->reserved[cairo_local_slot] = NULL;
}

// cairo_local_register(string $class, string $property): void
PHP_FUNCTION(cairo_local_register)
{
	zend_string *class_name, *name;
	ZEND_PARSE_PARAMETERS_START(2, 2)
		Z_PARAM_STR(class_name)
		Z_PARAM_STR(name)
	ZEND_PARSE_PARAMETERS_END();
	if (cairo_live != NULL || (cairo_requests_init && zend_hash_num_elements(&cairo_requests) > 0)) {
		zend_throw_error(NULL, "request-local statics must be registered before the server starts");
		RETURN_THROWS();
	}
	zend_class_entry *ce = zend_lookup_class(class_name);
	if (ce == NULL) {
		zend_throw_error(NULL, "class %s does not exist", ZSTR_VAL(class_name));
		RETURN_THROWS();
	}
	// Any visibility: the slot is only ever swapped, never read through PHP.
	const zend_class_entry *scope = EG(fake_scope);
	EG(fake_scope) = ce;
	zend_property_info *info = NULL;
	zval *slot = zend_std_get_static_property_with_info(ce, name, BP_VAR_W, &info);
	EG(fake_scope) = scope;
	if (slot == NULL) {
		RETURN_THROWS();  // it threw: no such static property
	}
	for (uint32_t i = 0; i < cairo_slot_count; i++) {
		if (cairo_slots[i] == slot) {
			return;  // already registered (a subclass shares its parent's static)
		}
	}
	cairo_slots = safe_erealloc(cairo_slots, cairo_slot_count + 1, sizeof(zval *), 0);
	cairo_slots[cairo_slot_count] = slot;
	cairo_boot.values = safe_erealloc(cairo_boot.values, cairo_slot_count + 1, sizeof(zval), 0);
	ZVAL_UNDEF(&cairo_boot.values[cairo_slot_count]);
	cairo_slot_count++;
	if (!cairo_requests_init) {
		zend_hash_init(&cairo_requests, 64, NULL, NULL, 0);
		cairo_requests_init = true;
	}
}

void cairo_local_bind_fiber(zend_fiber *f, zend_long request)
{
	if (cairo_local_slot < 0 || cairo_slot_count == 0) {
		return;
	}
	cairo_free_dead();
	cairo_local_store *s = NULL;
	if (request != 0) {
		s = zend_hash_index_find_ptr(&cairo_requests, (zend_ulong)request);
		if (s == NULL) {
			s = cairo_new_store();  // the request's reference
			zend_hash_index_add_new_ptr(&cairo_requests, (zend_ulong)request, s);
		}
		s->refs++;  // the fiber's
	}
	cairo_local_store *old = f->context.reserved[cairo_local_slot];
	f->context.reserved[cairo_local_slot] = s;
	if (f == EG(active_fiber)) {
		// Rebinding the running fiber: move the slots to the new store now.
		cairo_local_switch(&f->context, &f->context);
	}
	cairo_release(old);
}

// cairo_local_end(int $request): void. The request is over: its values
// are freed once none of its fibers can run any more.
PHP_FUNCTION(cairo_local_end)
{
	zend_long request;
	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_LONG(request)
	ZEND_PARSE_PARAMETERS_END();
	if (!cairo_requests_init) {
		return;
	}
	cairo_local_store *s = zend_hash_index_find_ptr(&cairo_requests, (zend_ulong)request);
	if (s != NULL) {
		zend_hash_index_del(&cairo_requests, (zend_ulong)request);
		cairo_release(s);
	}
	cairo_free_dead();
}

void cairo_local_request_end(void)
{
	cairo_slots = NULL;
	cairo_slot_count = 0;
	cairo_requests_init = false;
	cairo_boot = (cairo_local_store){0};
	cairo_live = cairo_dead = NULL;
}

void cairo_fiber_local_startup(void)
{
	cairo_local_slot = zend_get_resource_handle("cairo request-local");
	if (cairo_local_slot >= 0) {
		zend_observer_fiber_switch_register(cairo_local_switch);
		zend_observer_fiber_destroy_register(cairo_local_destroy);
	}
}

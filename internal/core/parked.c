// The fibers that wait on their PHP thread, and their timers. See parked.h.

#include <stdint.h>
#include <time.h>

#include <php.h>
#include <zend_fibers.h>

#include "fibers.h"
#include "loop.h"
#include "parked.h"

// One wait. It lives on the stack of the fiber that waits, inside
// cairo_park(), and is listed in the tables below from before the fiber
// suspends until the wait is woken or, when something else resumed the
// fiber, until cairo_park() returns: nothing points to it after that.
typedef struct parked {
	zend_long id;
	zend_long request;
	zend_fiber *fiber;            // with a reference
	zend_object *context;         // what it waits under; the fiber's own frame holds it
	int64_t at;                   // when its time is up, on the monotonic clock; INT64_MAX: never
	bool listed;
	struct parked *prev, *next;   // the other waits of its request
} parked;

typedef struct {
	int64_t at;
	zend_long id;
} timer;

// All per thread, in the memory of its PHP request, which ends with the
// thread: timers left behind count against its memory limits.
static __thread bool tables;         // the two below are initialized
static __thread HashTable waits;     // wait id => its parked
static __thread HashTable requests;  // request id => the request's first parked
static __thread timer *timers;       // a heap, the first one due on top
static __thread uint32_t ntimers, timers_cap;
static __thread uint32_t timed;      // listed waits that have a timer: the other timers are of waits that ended early

static int64_t mono_ns(void)
{
	struct timespec ts;
	clock_gettime(CLOCK_MONOTONIC, &ts);
	return (int64_t)ts.tv_sec * 1000000000 + ts.tv_nsec;
}

// Waits whose time is up at the same moment wake in the order they began.
static bool sooner(timer a, timer b)
{
	return a.at < b.at || (a.at == b.at && a.id < b.id);
}

static void sift_down(uint32_t i)
{
	timer t = timers[i];
	for (uint32_t child; (child = 2 * i + 1) < ntimers; i = child) {
		if (child + 1 < ntimers && sooner(timers[child + 1], timers[child])) {
			child++;
		}
		if (!sooner(timers[child], t)) {
			break;
		}
		timers[i] = timers[child];
	}
	timers[i] = t;
}

// A wait that ended early leaves its timer behind until it is due: drop
// those once they outnumber the timers still awaited.
static void drop_stale_timers(void)
{
	uint32_t kept = 0;
	for (uint32_t i = 0; i < ntimers; i++) {
		if (zend_hash_index_exists(&waits, (zend_ulong)timers[i].id)) {
			timers[kept++] = timers[i];
		}
	}
	ntimers = kept;
	for (uint32_t i = ntimers / 2; i-- > 0;) {
		sift_down(i);
	}
}

static void add_timer(int64_t at, zend_long id)
{
	if (ntimers > 2 * timed + 64) {
		drop_stale_timers();
	}
	if (ntimers == timers_cap) {
		timers_cap = timers_cap == 0 ? 64 : timers_cap * 2;
		timers = safe_erealloc(timers, timers_cap, sizeof(*timers), 0);
	}
	timer t = {.at = at, .id = id};
	uint32_t i = ntimers++;
	for (uint32_t up; i > 0 && sooner(t, timers[up = (i - 1) / 2]); i = up) {
		timers[i] = timers[up];
	}
	timers[i] = t;
}

static void list(parked *p)
{
	parked *first = zend_hash_index_find_ptr(&requests, (zend_ulong)p->request);
	p->next = first;
	if (first != NULL) {
		first->prev = p;
	}
	zend_hash_index_update_ptr(&requests, (zend_ulong)p->request, p);
	p->listed = true;
	if (p->at != INT64_MAX) {
		timed++;
		add_timer(p->at, p->id);
	}
}

static void unlist(parked *p)
{
	zend_hash_index_del(&waits, (zend_ulong)p->id);
	if (p->next != NULL) {
		p->next->prev = p->prev;
	}
	if (p->prev != NULL) {
		p->prev->next = p->next;
	} else if (p->next != NULL) {
		zend_hash_index_update_ptr(&requests, (zend_ulong)p->request, p->next);
	} else {
		zend_hash_index_del(&requests, (zend_ulong)p->request);
	}
	if (p->at != INT64_MAX) {
		timed--;
	}
	p->listed = false;
}

void cairo_parked_wake(zend_long id)
{
	parked *p = tables ? zend_hash_index_find_ptr(&waits, (zend_ulong)id) : NULL;
	if (p != NULL) {
		unlist(p);
		cairo_fibers_queue(p->fiber);
	}
}

int64_t cairo_parked_tick(void)
{
	if (ntimers == 0) {
		return -1;
	}
	int64_t now = mono_ns();
	while (ntimers > 0 && timers[0].at <= now) {
		zend_long id = timers[0].id;
		timers[0] = timers[--ntimers];
		if (ntimers > 0) {
			sift_down(0);
		}
		cairo_parked_wake(id);
	}
	return ntimers == 0 ? -1 : timers[0].at - now;
}

void cairo_park_fiber(zend_long id, zend_object *context, zend_long request, int64_t at, int kind)
{
	zval ret;
	zend_fiber *fiber = EG(active_fiber);
	if (fiber == NULL) {
		cairo_fiber_suspend(&ret);  // throws: there is no fiber to park
		return;
	}
	if (!tables) {
		zend_hash_init(&waits, 64, NULL, NULL, 0);
		zend_hash_init(&requests, 64, NULL, NULL, 0);
		tables = true;
	}
	parked p = {.id = id, .request = request, .fiber = fiber, .context = context, .at = at};
	if (zend_hash_index_add_ptr(&waits, (zend_ulong)id, &p) == NULL) {
		zend_throw_error(NULL, "cairo_park(): wait " ZEND_LONG_FMT " is taken", id);
		return;
	}
	list(&p);
	GC_ADDREF(&fiber->std);
	int64_t began = cairo_wait_began(request);
	cairo_fiber_suspend(&ret);
	zval_ptr_dtor(&ret);
	if (began != 0) {
		cairo_wait_ended(request, kind, began);
	}
	if (p.listed) {
		unlist(&p);  // not woken: the fiber could not suspend, is being destroyed, or was resumed by other code
	}
	OBJ_RELEASE(&fiber->std);
}

// cairo_park(int $id, object $context, int $request, int $at, int $kind): void
// Parks the running fiber as wait $id, which no other wait of the thread
// ever had, under $context of request $request: it runs again at the
// loop's next turn after cairo_wake($id), or once the clock that hrtime()
// reads is at $at (PHP_INT_MAX: never). $kind is what it waits in, for the
// probe of its request (the constants of Waits).
PHP_FUNCTION(cairo_park)
{
	zend_long id, request, at, kind;
	zend_object *context;
	ZEND_PARSE_PARAMETERS_START(5, 5)
		Z_PARAM_LONG(id)
		Z_PARAM_OBJ(context)
		Z_PARAM_LONG(request)
		Z_PARAM_LONG(at)
		Z_PARAM_LONG(kind)
	ZEND_PARSE_PARAMETERS_END();
	cairo_park_fiber(id, context, request, at, (int)kind);
}

// cairo_wake(int $id): void
// Ends wait $id if it still goes on: its fiber runs at the loop's next turn.
PHP_FUNCTION(cairo_wake)
{
	zend_long id;
	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_LONG(id)
	ZEND_PARSE_PARAMETERS_END();
	cairo_parked_wake(id);
}

// cairo_parked(): int
// How many fibers are parked on this thread and not woken yet.
PHP_FUNCTION(cairo_parked)
{
	ZEND_PARSE_PARAMETERS_NONE();
	RETURN_LONG(tables ? zend_hash_num_elements(&waits) : 0);
}

// cairo_parked_under(int $request): array
// The waits of request $request that go on: wait id => the context it is
// under.
PHP_FUNCTION(cairo_parked_under)
{
	zend_long request;
	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_LONG(request)
	ZEND_PARSE_PARAMETERS_END();
	parked *p = tables ? zend_hash_index_find_ptr(&requests, (zend_ulong)request) : NULL;
	if (p == NULL) {
		RETURN_EMPTY_ARRAY();
	}
	array_init(return_value);
	for (; p != NULL; p = p->next) {
		zval context;
		ZVAL_OBJ_COPY(&context, p->context);
		zend_hash_index_add_new(Z_ARRVAL_P(return_value), (zend_ulong)p->id, &context);
	}
}

// offload() jobs between PHP threads, without Go. See offload.h.

#include <stdint.h>
#include <stdlib.h>
#include <string.h>

#include <php.h>

#include "extension.h"
#include "loop.h"
#include "offload.h"

// An offload thread and the job it runs. The thread writes it, the threads
// that send jobs read job as a hint, and Go takes it once the thread has
// ended.
typedef struct {
	int tid;
	int32_t from;     // the thread whose fiber waits for the job,
	int64_t request;  // the request of that fiber, and
	int64_t job;      // the job's id; 0 while the thread runs none
} worker;

// What a job's message and its result's begin with, before their bytes.
typedef struct {
	int64_t job;
	int64_t request;
	int32_t from;
} job_head;

static worker *pool;      // set before the PHP threads start
static int npool;
static int64_t last_job;  // job ids are the process's, so that a thread never takes another's result for its own
static __thread worker *me;

int cairo_offload_pool(const int *tids, int n)
{
	worker *w = n > 0 ? calloc((size_t)n, sizeof(*w)) : NULL;
	if (n > 0 && w == NULL) {
		return -1;
	}
	for (int i = 0; i < n; i++) {
		w[i].tid = tids[i];
	}
	free(pool);
	pool = w;
	npool = n;
	return 0;
}

static worker *worker_of(int tid)
{
	for (int i = 0; i < npool; i++) {
		if (pool[i].tid == tid) {
			return &pool[i];
		}
	}
	return NULL;
}

// The offload thread with the least to do, or NULL when the queue of each
// is full.
static worker *least_busy(void)
{
	worker *best = NULL;
	int64_t min = CAIRO_OFFLOAD_QUEUE;
	for (int i = 0; i < npool; i++) {
		cairo_mailbox *box = cairo_mailbox_of(pool[i].tid);
		int64_t n = __atomic_load_n(&box->pushed, __ATOMIC_RELAXED) - __atomic_load_n(&box->took, __ATOMIC_RELAXED)
			+ (__atomic_load_n(&pool[i].job, __ATOMIC_RELAXED) != 0);
		if (n < min) {
			best = &pool[i];
			min = n;
		}
	}
	return best;
}

// A message of kind with head and bytes, or NULL: it would pass 2 GB, or
// there is no memory for it.
static cairo_msg *message(int kind, int64_t id, const job_head *head, const zend_string *bytes)
{
	size_t n = sizeof(*head) + ZSTR_LEN(bytes);
	cairo_msg *m = ZSTR_LEN(bytes) > INT32_MAX - sizeof(*head) ? NULL : malloc(sizeof(*m) + n);
	if (m == NULL) {
		return NULL;
	}
	m->id = id;
	m->kind = kind;
	m->len = (uint32_t)n;
	m->blen = 0;
	memcpy(m->data, head, sizeof(*head));
	memcpy(m->data + sizeof(*head), ZSTR_VAL(bytes), ZSTR_LEN(bytes));
	return m;
}

int cairo_offload_event(int tid, const cairo_msg *m, zval *event)
{
	job_head head;
	if (m->len < sizeof(head)) {
		return FAILURE;
	}
	memcpy(&head, m->data, sizeof(head));
	const char *bytes = m->data + sizeof(head);
	size_t n = m->len - sizeof(head);
	array_init_size(event, 3);
	switch (m->kind) {
	case CAIRO_EVENT_JOB:
		if (me == NULL && (me = worker_of(tid)) == NULL) {
			break;   // not an offload thread: nobody sends it a job
		}
		me->from = head.from;
		me->request = head.request;
		__atomic_store_n(&me->job, head.job, __ATOMIC_SEQ_CST);
		add_assoc_string(event, "type", "offload");
		add_assoc_stringl(event, "payload", bytes, n);
		return SUCCESS;
	case CAIRO_EVENT_DONE:
		add_assoc_string(event, "type", "offloaded");
		add_assoc_long(event, "id", (zend_long)head.job);
		add_assoc_stringl(event, "result", bytes, n);
		return SUCCESS;
	}
	zval_ptr_dtor(event);
	return FAILURE;
}

int64_t cairo_offload_lost(int tid, int *from)
{
	worker *w = worker_of(tid);
	if (w == NULL) {
		return 0;
	}
	*from = w->from;
	return __atomic_exchange_n(&w->job, 0, __ATOMIC_SEQ_CST);
}

// cairo_offload_send(string $job, int $request): int
// Queues $job, which a fiber of $request on this thread serialized, for the
// offload thread with the least to do, and returns the id its result will
// carry: an event ['type' => 'offloaded', 'id' => ..., 'result' => ...] for
// this thread, or with 'error' in place of the result when the offload
// thread stopped first. Returns 0 when every offload thread has
// CAIRO_OFFLOAD_QUEUE events waiting: the caller asks again later.
PHP_FUNCTION(cairo_offload_send)
{
	zend_string *job;
	zend_long request;
	ZEND_PARSE_PARAMETERS_START(2, 2)
		Z_PARAM_STR(job)
		Z_PARAM_LONG(request)
	ZEND_PARSE_PARAMETERS_END();
	int tid = cairo_thread_id();
	if (tid < 0 || npool == 0) {
		zend_throw_error(NULL, "cairo_offload_send(): this thread has no offload threads to send to");
		RETURN_THROWS();
	}
	worker *w = least_busy();
	if (w == NULL) {
		RETURN_LONG(0);
	}
	job_head head = {.job = __atomic_add_fetch(&last_job, 1, __ATOMIC_SEQ_CST), .request = request, .from = tid};
	cairo_msg *m = message(CAIRO_EVENT_JOB, head.job, &head, job);
	if (m == NULL) {
		zend_throw_error(NULL, "cairo_offload_send(): the job is larger than 2 GB, or there is no memory for it");
		RETURN_THROWS();
	}
	cairo_inbox_queue(w->tid, m);
	RETURN_LONG((zend_long)head.job);
}

// cairo_offload_done(string $result): void
// The job this offload thread took last is done: queues its serialized
// result for the thread that sent it. When that thread has ended meanwhile,
// the result is the next thread's under its id, where nobody waits for it.
PHP_FUNCTION(cairo_offload_done)
{
	zend_string *result;
	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_STR(result)
	ZEND_PARSE_PARAMETERS_END();
	if (me == NULL || me->job == 0) {
		zend_throw_error(NULL, "cairo_offload_done(): this thread runs no job");
		RETURN_THROWS();
	}
	// The event is the request's, as the result of a Go operation is.
	job_head head = {.job = me->job, .request = me->request, .from = me->from};
	cairo_msg *m = message(CAIRO_EVENT_DONE, head.request, &head, result);
	if (m == NULL) {
		// The job stays the thread's: ending with it is what tells the sender.
		zend_throw_error(NULL, "cairo_offload_done(): the result is larger than 2 GB, or there is no memory for it");
		RETURN_THROWS();
	}
	__atomic_store_n(&me->job, 0, __ATOMIC_SEQ_CST);
	cairo_inbox_queue(head.from, m);
}

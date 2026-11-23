// Cairo's bridge between Go and an embedded, thread-safe (ZTS) PHP.
//
// Cairo is its own PHP SAPI ("cairo", below): one boot thread runs the
// declare phase, then each PHP worker thread gets its own interpreter context
// (ts_resource) and runs the Cairo runtime script, which loops on
// cairo_next_event() and runs every request in its own Fiber.

#include <pthread.h>
#include <limits.h>
#include <stdatomic.h>
#include <stdlib.h>
#include <string.h>

#include <php.h>
#include <php_main.h>
#include <main/build-defs.h>
#include <SAPI.h>
#include <TSRM.h>
#include <zend_exceptions.h>
#include <zend_fibers.h>
#include <zend_observer.h>
#include <zend_extensions.h>
#include <zend_stream.h>
#include <php_network.h>
#include <ext/json/php_json.h>
#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <unistd.h>
#include <php_variables.h>
#include <signal.h>
#include <ext/standard/php_fopen_wrappers.h>

#include "cairo.h"
#include "extension.h"
#include "outbox.h"
#include "wire.h"
#include "sockets.h"
#include "offload.h"
#include "waitops.h"
#include "fiber_local.h"
#include "fibers.h"
#include "parked.h"
#include "_cgo_export.h"

// -1 is the boot thread (declare phase); 0..n-1 are worker threads.
static __thread int cairo_tid = -1;

int cairo_thread_id(void)
{
	return cairo_tid;
}

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_none, 0, 0, 0)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_spec, 0, 0, 1)
	ZEND_ARG_INFO(0, json)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_respond, 0, 0, 2)
	ZEND_ARG_INFO(0, id)
	ZEND_ARG_INFO(0, json)
	ZEND_ARG_INFO(0, body)
ZEND_END_ARG_INFO()

// cairo_info(): string (JSON: mode, server file, thread id)
PHP_FUNCTION(cairo_info)
{
	ZEND_PARSE_PARAMETERS_NONE();
	char *s = goCairoInfo(cairo_tid);
	RETVAL_STRING(s);
	free(s);
}

// cairo_spec(string $json): void. Declare phase: records the spec.
// Serve phase: checks this thread built the same spec as the boot thread.
PHP_FUNCTION(cairo_spec)
{
	char *json;
	size_t len;
	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_STRING(json, len)
	ZEND_PARSE_PARAMETERS_END();
	char *err = goCairoSpec(cairo_tid, json, (int)len);
	if (err != NULL) {
		zend_throw_exception(NULL, err, 0);
		free(err);
	}
}

// cairo_respond(int $id, string $json, string $body = ''): void. $json is
// the answer's metadata; $body, an HTTP response's body, goes to Go as raw
// bytes.
PHP_FUNCTION(cairo_respond)
{
	zend_long id;
	char *json, *body = NULL;
	size_t len, blen = 0;
	ZEND_PARSE_PARAMETERS_START(2, 3)
		Z_PARAM_LONG(id)
		Z_PARAM_STRING(json, len)
		Z_PARAM_OPTIONAL
		Z_PARAM_STRING(body, blen)
	ZEND_PARSE_PARAMETERS_END();
	if (len > INT_MAX || blen > INT_MAX) {
		zend_value_error("cairo_respond(): the answer is larger than 2 GB");
		RETURN_THROWS();
	}
	if (cairo_answer(id, json, len, body, blen) != 0) {
		zend_throw_error(NULL, "cairo_respond(): no memory for the answer");
		RETURN_THROWS();
	}
}

// cairo_retire(string $reason): void. Asks Go to replace this request thread
// with a fresh one; Go then sends a "retire" event, after which the thread
// finishes the requests it holds and exits.
PHP_FUNCTION(cairo_retire)
{
	char *reason;
	size_t len;
	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_STRING(reason, len)
	ZEND_PARSE_PARAMETERS_END();
	goThreadRetire(cairo_tid, reason, (int)len);
}

// cairo_go_call(string $op, array $args): array. Runs a Go operation on
// this thread, outside a fiber; returns ['result' => ...] or ['error' => ...].
ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_go_call, 0, 0, 2)
	ZEND_ARG_INFO(0, op)
	ZEND_ARG_INFO(0, args)
ZEND_END_ARG_INFO()

PHP_FUNCTION(cairo_go_call)
{
	zend_string *name;
	HashTable *args;
	ZEND_PARSE_PARAMETERS_START(2, 2)
		Z_PARAM_STR(name)
		Z_PARAM_ARRAY_HT(args)
	ZEND_PARSE_PARAMETERS_END();
	smart_str encoded = {0};
	if (cairo_wire_encode(args, &encoded) == FAILURE) {
		smart_str_free(&encoded);
		RETURN_THROWS();
	}
	if (ZSTR_LEN(encoded.s) > INT_MAX) {
		smart_str_free(&encoded);
		zend_value_error("cairo_go_call(): the arguments are larger than 2 GB");
		RETURN_THROWS();
	}
	int n = 0;
	char *res = goCall(ZSTR_VAL(name), ZSTR_VAL(encoded.s), (int)ZSTR_LEN(encoded.s), &n);
	smart_str_free(&encoded);
	int ok = cairo_wire_decode(res, n < 0 ? 0 : (size_t)n, return_value);
	free(res);
	if (ok == FAILURE) {
		zend_throw_exception(NULL, "cairo: malformed answer from Go", 0);
		RETURN_THROWS();
	}
}

PHP_FUNCTION(cairo_guard);
PHP_FUNCTION(cairo_next_event);

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_retire, 0, 0, 1)
	ZEND_ARG_INFO(0, reason)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_next_event, 0, 0, 1)
	ZEND_ARG_INFO(0, idle)
	ZEND_ARG_INFO(0, turn)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_guard, 0, 0, 1)
	ZEND_ARG_INFO(0, fn)
ZEND_END_ARG_INFO()

static const zend_function_entry cairo_functions[] = {
	PHP_FE(cairo_info, arginfo_cairo_none)
	PHP_FE(cairo_spec, arginfo_cairo_spec)
	PHP_FE(cairo_next_event, arginfo_cairo_next_event)
	PHP_FE(cairo_respond, arginfo_cairo_respond)
	PHP_FE(cairo_retire, arginfo_cairo_retire)
	PHP_FE(cairo_wait_socket, arginfo_cairo_wait_socket)
	PHP_FE(cairo_wait_sockets, arginfo_cairo_wait_sockets)
	PHP_FE(cairo_wait_here, arginfo_cairo_wait_here)
	PHP_FE(cairo_offload_send, arginfo_cairo_offload_send)
	PHP_FE(cairo_offload_done, arginfo_cairo_offload_done)
	PHP_FE(cairo_fiber_call, arginfo_cairo_fiber_call)
	PHP_FE(cairo_op_cancel, arginfo_cairo_op_cancel)
	PHP_FE(cairo_go_call, arginfo_cairo_go_call)
	PHP_FE(cairo_guard, arginfo_cairo_guard)
	PHP_FE(cairo_local_register, arginfo_cairo_local_register)
	PHP_FE(cairo_fiber_context, arginfo_cairo_fiber_context)
	PHP_FE(cairo_fiber_bind, arginfo_cairo_fiber_bind)
	PHP_FE(cairo_fiber_reuse, arginfo_cairo_fiber_reuse)
	PHP_FE(cairo_fiber_idle, arginfo_cairo_fiber_idle)
	PHP_FE(cairo_ready, arginfo_cairo_ready)
	PHP_FE(cairo_run_ready, arginfo_cairo_run_ready)
	PHP_FE(cairo_park, arginfo_cairo_park)
	PHP_FE(cairo_wake, arginfo_cairo_wake)
	PHP_FE(cairo_parked, arginfo_cairo_parked)
	PHP_FE(cairo_parked_under, arginfo_cairo_parked_under)
	PHP_FE(cairo_local_end, arginfo_cairo_local_end)
	PHP_FE_END
};

// ---- Per-fiber engine state -----------------------------------------------
//
// Some PHP code switches the engine's error mode for the length of a call:
// PDO's constructor turns warnings into PDOException while it connects. With
// hooks, that call can suspend its fiber, and the mode, a per-thread global,
// would leak into whichever fiber runs next (and be "restored" wrongly
// later). Cairo keeps the mode per fiber and swaps it on every fiber switch.

typedef struct {
	zend_error_handling_t error_handling;
	zend_class_entry *exception_class;
	int guards;    // cairo_guard() calls running in this fiber
	bool stopped;  // Context\DeadlineExceeded was thrown here at the deadline
	bool unwound;  // then an unwind (exit) that cairo_guard() turns back into it
} cairo_fiber_state;

static int cairo_fiber_slot = -1;

static void cairo_fiber_switch(zend_fiber_context *from, zend_fiber_context *to)
{
	cairo_fiber_state *st = from->reserved[cairo_fiber_slot];
	if (from->status == ZEND_FIBER_STATUS_DEAD) {
		free(st);
		from->reserved[cairo_fiber_slot] = NULL;
	} else {
		if (st == NULL) {
			st = calloc(1, sizeof(*st));
			from->reserved[cairo_fiber_slot] = st;
		}
		if (st != NULL) {
			st->error_handling = EG(error_handling);
			st->exception_class = EG(exception_class);
		}
	}
	cairo_fiber_state *next = to->reserved[cairo_fiber_slot];
	if (next != NULL) {
		EG(error_handling) = next->error_handling;
		EG(exception_class) = next->exception_class;
	} else if (to->status == ZEND_FIBER_STATUS_INIT) {
		// A new fiber starts with PHP's normal error handling.
		EG(error_handling) = EH_NORMAL;
		EG(exception_class) = NULL;
	}
}

// --- Hard stop at the deadline ---------------------------------------------
//
// Code that never suspends its fiber (a CPU loop) cannot see the deadline
// through Go. When a request's deadline passes, Go calls cairo_interrupt()
// for its thread every few milliseconds until PHP answers. It raises the
// thread's VM interrupt flag, and at the next safe point (a loop back-edge or
// a call) the engine runs cairo_on_interrupt in whatever fiber is running.
// Cairo\Internal\interrupt() says whether that fiber's request is past its
// deadline, not yet answered, and running user code (not Cairo's runtime).
// If so, the first time Context\DeadlineExceeded is thrown, so the code can
// clean up. If the code catches it and carries on, the next time the fiber is
// unwound the way exit() does it (catch and finally blocks are skipped,
// destructors run), and cairo_guard(), around the handler and each async()
// task, turns the unwind back into Context\DeadlineExceeded for the runtime.

typedef struct {
#if PHP_VERSION_ID >= 80200
	_Atomic(zend_atomic_bool *) vm_interrupt;
#else
	_Atomic(volatile zend_bool *) vm_interrupt;
#endif
	atomic_bool poked;   // by cairo_interrupt(): a deadline has passed
} cairo_thread_slot;

static cairo_thread_slot cairo_slots[CAIRO_MAX_THREADS];
static void (*cairo_prev_interrupt)(zend_execute_data *execute_data);

static void cairo_interrupt_register(void)
{
	if (cairo_tid >= 0 && cairo_tid < CAIRO_MAX_THREADS) {
		atomic_store(&cairo_slots[cairo_tid].poked, false);
		atomic_store(&cairo_slots[cairo_tid].vm_interrupt, &EG(vm_interrupt));
	}
}

static void cairo_interrupt_unregister(void)
{
	if (cairo_tid >= 0 && cairo_tid < CAIRO_MAX_THREADS) {
		atomic_store(&cairo_slots[cairo_tid].vm_interrupt, NULL);
	}
}

// Has thread tid run cairo_on_interrupt at its next safe point. Called from
// any goroutine.
void cairo_vm_interrupt(int tid)
{
	if (tid < 0 || tid >= CAIRO_MAX_THREADS) {
		return;
	}
#if PHP_VERSION_ID >= 80200
	zend_atomic_bool *flag = atomic_load(&cairo_slots[tid].vm_interrupt);
	if (flag != NULL) {
		zend_atomic_bool_store(flag, true);
	}
#else
	volatile zend_bool *flag = atomic_load(&cairo_slots[tid].vm_interrupt);
	if (flag != NULL) {
		*flag = 1;
	}
#endif
}

// Called from any goroutine.
void cairo_interrupt(int tid)
{
	if (tid < 0 || tid >= CAIRO_MAX_THREADS) {
		return;
	}
	atomic_store(&cairo_slots[tid].poked, true);
	cairo_vm_interrupt(tid);
}

// The running fiber's Cairo state, created when missing.
static cairo_fiber_state *cairo_current_fiber_state(void)
{
	if (cairo_fiber_slot < 0 || EG(active_fiber) == NULL) {
		return NULL;
	}
	zend_fiber_context *ctx = &EG(active_fiber)->context;
	cairo_fiber_state *st = ctx->reserved[cairo_fiber_slot];
	if (st == NULL) {
		st = calloc(1, sizeof(*st));
		if (st != NULL) {
			st->error_handling = EG(error_handling);
			st->exception_class = EG(exception_class);
		}
		ctx->reserved[cairo_fiber_slot] = st;
	}
	return st;
}

static void cairo_throw_deadline(void)
{
	zend_string *name = zend_string_init("Cairo\\Context\\DeadlineExceeded", sizeof("Cairo\\Context\\DeadlineExceeded") - 1, 0);
	zend_class_entry *ce = zend_lookup_class(name);
	zend_string_release(name);
	zend_throw_exception(ce != NULL ? ce : zend_ce_exception, "context deadline exceeded", 0);
}

// Whether the running fiber should stop: Cairo\Internal\interrupt($file).
static bool cairo_should_stop(void)
{
	zend_string *file = zend_get_executed_filename_ex();
	zval fn, ret, arg;
	ZVAL_STRING(&fn, "Cairo\\Internal\\interrupt");
	if (file != NULL) {
		ZVAL_STR_COPY(&arg, file);
	} else {
		ZVAL_NULL(&arg);
	}
	ZVAL_UNDEF(&ret);
	bool stop = call_user_function(NULL, NULL, &fn, &ret, 1, &arg) == SUCCESS
		&& EG(exception) == NULL && Z_TYPE(ret) == IS_TRUE;
	zval_ptr_dtor(&fn);
	zval_ptr_dtor(&arg);
	zval_ptr_dtor(&ret);
	return stop;
}

static void cairo_on_interrupt(zend_execute_data *execute_data)
{
	if (cairo_tid >= 0 && cairo_tid < CAIRO_MAX_THREADS && EG(active_fiber) != NULL && EG(exception) == NULL) {
		cairo_extensions_interrupt(cairo_tid);
	}
	if (cairo_tid >= 0 && cairo_tid < CAIRO_MAX_THREADS
		&& EG(active_fiber) != NULL && EG(exception) == NULL
		&& atomic_exchange(&cairo_slots[cairo_tid].poked, false)) {
		cairo_fiber_state *st = cairo_current_fiber_state();
		if (st != NULL && cairo_should_stop()) {
			if (st->stopped && st->guards > 0) {
				st->unwound = true;
				zend_throw_unwind_exit();
			} else {
				st->stopped = true;
				cairo_throw_deadline();
			}
		}
	}
	if (cairo_prev_interrupt != NULL) {
		cairo_prev_interrupt(execute_data);
	}
}

// cairo_guard(callable $fn): mixed calls $fn; an unwind started at the
// deadline ends here and becomes Context\DeadlineExceeded.
PHP_FUNCTION(cairo_guard)
{
	zend_fcall_info fci;
	zend_fcall_info_cache fcc;
	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_FUNC(fci, fcc)
	ZEND_PARSE_PARAMETERS_END();
	cairo_fiber_state *st = cairo_current_fiber_state();
	if (st != NULL) {
		st->guards++;
	}
	fci.retval = return_value;
	zend_call_function(&fci, &fcc);
	if (st != NULL) {
		st->guards--;
		if (st->guards == 0 && st->unwound && EG(exception) != NULL && zend_is_unwind_exit(EG(exception))) {
			st->unwound = false;
			zend_clear_exception();
			zval_ptr_dtor(return_value);
			ZVAL_NULL(return_value);
			cairo_throw_deadline();
		}
		if (st->guards == 0) {
			// The guarded handler is over; the runtime may run the next
			// request in this same fiber, which starts unstopped.
			st->stopped = false;
			st->unwound = false;
		}
	}
}

// Module startup: the extensions install their hooks here, once, before any
// worker thread copies the function table.
static void cairo_opcache_disguise(void);

static PHP_MINIT_FUNCTION(cairo)
{
	cairo_opcache_disguise();
	if (cairo_extensions_startup() == FAILURE) {
		return FAILURE;
	}
	cairo_fiber_local_startup();
	if (cairo_fibers_startup() == FAILURE) {
		return FAILURE;
	}
	cairo_fiber_slot = zend_get_resource_handle("cairo");
	if (cairo_fiber_slot >= 0) {
		zend_observer_fiber_switch_register(cairo_fiber_switch);
	}
	cairo_prev_interrupt = zend_interrupt_function;
	zend_interrupt_function = cairo_on_interrupt;
	return SUCCESS;
}

static zend_module_entry cairo_module_entry = {
	STANDARD_MODULE_HEADER,
	"cairo",
	cairo_functions,
	PHP_MINIT(cairo), NULL, NULL, NULL, NULL,
	"0.1.0-dev",
	STANDARD_MODULE_PROPERTIES
};

// --- The cairo SAPI ---------------------------------------------------------
//
// Cairo is its own SAPI (PHP_SAPI is "cairo"), adapted from PHP's embed SAPI
// and FrankenPHP: the boot thread starts PHP and runs the declare phase, and
// each worker thread runs one long PHP request (cairo_thread_run). Requests
// never reach PHP through the SAPI; Go hands them to the runtime script.

// Defaults a php.ini may override (lowest precedence).
static void cairo_ini_defaults(HashTable *configuration_hash)
{
	static const char *defaults[][2] = {
		{"html_errors", "0"},
		{"implicit_flush", "1"},
		{"output_buffering", "0"},
		{"register_argc_argv", "0"},   // PHP 8.5 deprecates argv from the query string
		{"display_errors", "0"},   // errors go to the log (below), once
		{"log_errors", "1"},
	};
	for (size_t i = 0; i < sizeof(defaults) / sizeof(defaults[0]); i++) {
		zval v;
		ZVAL_NEW_STR(&v, zend_string_init(defaults[i][1], strlen(defaults[i][1]), 1));
		zend_hash_str_update(configuration_hash, defaults[i][0], strlen(defaults[i][0]), &v);
	}
}

// Settings no php.ini may change: each worker thread is one PHP request that
// lasts as long as the server, so PHP's own time limits would end it. Request
// deadlines are Cairo's (Middleware::timeout()).
static const char cairo_forced_ini[] =
	"max_execution_time=0\n"
	"max_input_time=-1\n";

static size_t cairo_ub_write(const char *str, size_t len)
{
	// Output outside an output buffer (isolated mode buffers each request)
	// goes to stdout, as with the CLI.
	size_t done = 0;
	while (done < len) {
		ssize_t n = write(STDOUT_FILENO, str + done, len - done);
		if (n <= 0) {
			if (n < 0 && errno == EINTR) {
				continue;
			}
			break;
		}
		done += (size_t)n;
	}
	return len;
}

static void cairo_flush(void *server_context)
{
}

static void cairo_send_header(sapi_header_struct *sapi_header, void *server_context)
{
}

static char *cairo_read_cookies(void)
{
	return NULL;
}

static void cairo_register_variables(zval *track_vars_array)
{
	php_import_environment_variables(track_vars_array);
}

// PHP's log lines (errors, warnings, error_log()) go to Cairo's logger,
// unless the error_log setting names a file.
static void cairo_log_message(const char *message, int syslog_type)
{
	goPHPLog(cairo_tid, (char *)message, syslog_type);
}

// ---- Opcache ------------------------------------------------------------------
//
// Cairo loads opcache (built as opcache.so next to PHP before PHP 8.5, built
// in from 8.5) unless CAIRO_OPCACHE=0 or php.ini already loads it. Opcache only starts under the SAPIs it knows,
// and checks the name once, while Zend extensions start; so the SAPI goes by
// "cli-server" for that moment (from the cairo module's MINIT, which runs just
// before) and gets its own name back afterwards. PHP_SAPI and php_sapi_name()
// keep reporting "cairo".

static const char *cairo_sapi_name;
static char *cairo_ini_entries;

static bool cairo_opcache_wanted(void)
{
	const char *env = getenv("CAIRO_OPCACHE");
	if (env != NULL && (strcmp(env, "0") == 0 || strcasecmp(env, "off") == 0 || strcasecmp(env, "false") == 0)) {
		return false;
	}
#if PHP_VERSION_ID >= 80500
	return true;
#else
	return access(PHP_EXTENSION_DIR "/opcache.so", R_OK) == 0;
#endif
}

static void cairo_opcache_disguise(void)
{
	if (cairo_sapi_name == NULL && cairo_ini_entries != NULL) {
		cairo_sapi_name = sapi_module.name;
		sapi_module.name = "cli-server";
	}
}

static void cairo_opcache_restore(void)
{
	if (cairo_sapi_name != NULL) {
		sapi_module.name = (char *)cairo_sapi_name;
		cairo_sapi_name = NULL;
	}
}

// Module startup registers the cairo module (and its functions), like any
// PHP extension, with opcache added to the INI entries when wanted.
static int cairo_sapi_startup(sapi_module_struct *sapi)
{
	if (cairo_opcache_wanted()) {
		// Cairo's fixed settings, plus opcache. INI entries are parsed
		// like php.ini lines, so zend_extension= loads it; a php.ini that loads
		// it too only gets a warning.
		const char *base = sapi->ini_entries != NULL ? sapi->ini_entries : "";
#if PHP_VERSION_ID >= 80500
		const char *add = "";   // built in: only the SAPI name below is needed
#else
		const char *add = "zend_extension=" PHP_EXTENSION_DIR "/opcache.so\n";
#endif
		cairo_ini_entries = malloc(strlen(base) + strlen(add) + 1);
		if (cairo_ini_entries != NULL) {
			strcpy(cairo_ini_entries, base);
			strcat(cairo_ini_entries, add);
			sapi->ini_entries = cairo_ini_entries;
		}
	}
	int rc = php_module_startup(sapi, &cairo_module_entry);
	cairo_opcache_restore();
	return rc;
}

static sapi_module_struct cairo_sapi_module = {
	"cairo",                      /* name */
	"Cairo",                      /* pretty name */

	cairo_sapi_startup,           /* startup */
	php_module_shutdown_wrapper,  /* shutdown */

	NULL,                         /* activate */
	NULL,                         /* deactivate */

	cairo_ub_write,               /* unbuffered write */
	cairo_flush,                  /* flush */
	NULL,                         /* get uid */
	NULL,                         /* getenv */

	php_error,                    /* error handler */

	NULL,                         /* header handler */
	NULL,                         /* send headers handler */
	cairo_send_header,            /* send header handler */

	NULL,                         /* read POST data */
	cairo_read_cookies,           /* read Cookies */

	cairo_register_variables,     /* register server variables */
	cairo_log_message,            /* log message */
	NULL,                         /* get request time */
	NULL,                         /* child terminate */

	STANDARD_SAPI_MODULE_PROPERTIES
};

static int run_file(const char *file)
{
	zend_file_handle fh;
	zend_stream_init_filename(&fh, file);
	bool ok = php_execute_script(&fh);
	zend_destroy_file_handle(&fh);
	if (!ok) {
		return -1;
	}
	return EG(exit_status);
}

// cairo_boot starts PHP (module startup, then a request on this thread for
// the declare phase) and runs the runtime script. php_ini is a php.ini path
// (file or directory), or NULL to search the usual places.
static void cairo_signals_onstack(void);

int cairo_boot(const char *runtime_file, const char *php_ini)
{
	static char *argv[] = {"cairo", NULL};
	// Sockets whose peer went away must not kill the process.
	signal(SIGPIPE, SIG_IGN);
#ifdef ZTS
	php_tsrm_startup();
#endif
	zend_signal_startup();
	sapi_startup(&cairo_sapi_module);   // clears ini_entries; set them after

	cairo_sapi_module.ini_defaults = cairo_ini_defaults;
	cairo_sapi_module.ini_entries = cairo_forced_ini;
	cairo_sapi_module.php_ini_ignore_cwd = 1;
	if (php_ini != NULL && php_ini[0] != '\0') {
		cairo_sapi_module.php_ini_path_override = strdup(php_ini);
	}
	cairo_sapi_module.executable_location = argv[0];

	if (cairo_sapi_module.startup(&cairo_sapi_module) == FAILURE) {
		return -1;
	}
	SG(options) |= SAPI_OPTION_NO_CHDIR;
	SG(request_info).argc = 1;
	SG(request_info).argv = argv;
	if (php_request_startup() == FAILURE) {
		php_module_shutdown();
		return -1;
	}
	cairo_signals_onstack();
	SG(headers_sent) = 1;
	SG(request_info).no_headers = 1;
	php_register_variable("PHP_SELF", "-", NULL);

	cairo_tid = -1;
	return run_file(runtime_file);
}

// PHP's signal layer (zend_signal) installs handlers without SA_ONSTACK
// and passes signals it does not use on to the handler it replaced, which is
// Go's. Go's handlers must run on the alternate signal stack and abort
// otherwise; Go's CPU profiler (SIGPROF) hit that. Handlers keep their
// function and only gain SA_ONSTACK. Signals are process-wide, so after the
// first PHP request startup this changes nothing.
static void cairo_signals_onstack(void)
{
	static const int sigs[] = {SIGPROF, SIGHUP, SIGINT, SIGQUIT, SIGTERM, SIGUSR1, SIGUSR2, SIGALRM};
	for (size_t i = 0; i < sizeof(sigs) / sizeof(sigs[0]); i++) {
		struct sigaction sa;
		if (sigaction(sigs[i], NULL, &sa) != 0 || (sa.sa_flags & SA_ONSTACK)
			|| sa.sa_handler == SIG_DFL || sa.sa_handler == SIG_IGN) {
			continue;
		}
		sa.sa_flags |= SA_ONSTACK;
		sigaction(sigs[i], &sa, NULL);
	}
}

void cairo_shutdown(void)
{
	php_request_shutdown(NULL);
	php_module_shutdown();
	sapi_shutdown();
#ifdef ZTS
	tsrm_shutdown();
#endif
}

int cairo_thread_run(int tid, const char *runtime_file)
{
	cairo_tid = tid;
	(void)ts_resource(0);
	if (php_request_startup() == FAILURE) {
		ts_free_thread();
		return -1;
	}
	cairo_signals_onstack();
	cairo_interrupt_register();
	int status = run_file(runtime_file);
	cairo_interrupt_unregister();
	php_request_shutdown(NULL);
	cairo_sockets_thread_end();
	cairo_extensions_thread_end();   // after the request closed what it held
	ts_free_thread();
	return status;
}

// PHP worker threads are created here in C, not by Go. PHP runs each fiber
// on its own C stack, and Go only accepts callbacks from a foreign stack on
// threads it did not create, so cairo_* functions called inside a fiber work.
struct cairo_thread_arg {
	int tid;
	char *file;
};

static void *cairo_thread_main(void *p)
{
	struct cairo_thread_arg *arg = p;
	// SIGPROF (Go's CPU profiler, a process-wide timer) goes to Go's own
	// threads: Go's handler aborts when it lands on this C thread while it
	// runs Go code from a callback. Cairo sets max_execution_time=0 and stops
	// PHP at deadlines itself, so PHP has no use for it here.
	sigset_t prof;
	sigemptyset(&prof);
	sigaddset(&prof, SIGPROF);
	pthread_sigmask(SIG_BLOCK, &prof, NULL);
	int status = cairo_thread_run(arg->tid, arg->file);
	int tid = arg->tid;
	free(arg->file);
	free(arg);
	cairo_outbox_exit(tid, status);
	return NULL;
}

int cairo_thread_start(int tid, const char *runtime_file)
{
	struct cairo_thread_arg *arg = malloc(sizeof(*arg));
	if (arg == NULL) {
		return -1;
	}
	arg->tid = tid;
	arg->file = strdup(runtime_file);

	pthread_attr_t attr;
	pthread_attr_init(&attr);
	pthread_attr_setstacksize(&attr, 16 * 1024 * 1024);
	pthread_attr_setdetachstate(&attr, PTHREAD_CREATE_DETACHED);
	pthread_t th;
	int rc = pthread_create(&th, &attr, cairo_thread_main, arg);
	pthread_attr_destroy(&attr);
	if (rc != 0) {
		free(arg->file);
		free(arg);
		return -1;
	}
	return 0;
}

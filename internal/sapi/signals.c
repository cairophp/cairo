// Signals for the scripts of `cairo php`: PHP's own pcntl says what a signal
// does, and Go is who receives it.
//
// A signal for the process lands on any of its threads, most often one of
// Go's, where PHP has nobody to record it for: PHP's handler passes it on
// to Go's, whose default ends the process, and the script's handler never
// runs. So for a signal that the script handles (pcntl_signal()) the system
// calls cairo_signal_handler(), which gives the signal to Go wherever it
// landed. Go's os/signal queues it, and signals.go brings it to the script's
// thread with cairo_signal_deliver():
//
//   - Under the scheduler a signal is handled inside a fiber. It is marked
//     pending, the thread is interrupted, and kicked out of a system call it
//     may be in (an interrupted sleep() returns early, as under PHP). The
//     fiber that runs, at its next safe point, or else the fiber that
//     Cairo\Internal\Signals keeps for it, raises the signal on its own
//     thread: this time it goes to PHP's handler, and pcntl runs the
//     script's on the way out.
//   - With no scheduler the kick itself goes to PHP's handler, as if the
//     system had chosen that thread.
//
// pcntl is left out of a server: a handler there would take the signals that
// stop it. What forks or waits for a signal with its thread is left out of
// both: a fork copies one thread of a process that has many, Go's among
// them, and no signal arrives on its own on a thread that waits for one.

#include <errno.h>
#include <pthread.h>
#include <signal.h>
#include <stdatomic.h>
#include <stdint.h>
#include <string.h>

#include <php.h>
#include <SAPI.h>

#include "signals.h"
#include "_cgo_export.h"

#define SIGNALS 64   // the signals of Linux: one bit each
#define BIT(signo) (UINT64_C(1) << ((signo) - 1))

typedef void (*handler)(int, siginfo_t *, void *);

static handler go_handlers[SIGNALS + 1];    // Go's handler of each signal
static handler php_handlers[SIGNALS + 1];   // PHP's, of each signal the script handles
static pthread_t script_thread;
static _Atomic uint64_t pending;   // signals Go received, for a fiber to raise
static _Atomic uint64_t kicked;    // the next one on the script's thread is Go's kick
static atomic_bool scheduled;      // the script's fibers handle its signals
static __thread volatile sig_atomic_t raising;   // the signal this thread raises for PHP
static zif_handler pcntl_signal_original;

static void cairo_signal_handler(int signo, siginfo_t *info, void *context)
{
	int saved = errno;
	if (raising == signo) {
		php_handlers[signo](signo, info, context);
	} else if ((atomic_load(&kicked) & BIT(signo)) && pthread_equal(pthread_self(), script_thread)) {
		atomic_fetch_and(&kicked, ~BIT(signo));
		// Under the scheduler the kick only ends a system call: a fiber raises the signal.
		if (!atomic_load(&scheduled)) {
			atomic_fetch_and(&pending, ~BIT(signo));
			php_handlers[signo](signo, info, context);
		}
	} else {
		go_handlers[signo](signo, info, context);
	}
	errno = saved;
}

// The handler the system calls for signo, or NULL when it calls none.
static handler cairo_signal_installed(int signo)
{
	struct sigaction sa;
	if (sigaction(signo, NULL, &sa) != 0 || !(sa.sa_flags & SA_SIGINFO)
		|| sa.sa_handler == SIG_DFL || sa.sa_handler == SIG_IGN) {
		return NULL;
	}
	return sa.sa_sigaction;
}

int cairo_signal_deliver(int signo)
{
	if (signo < 1 || signo > SIGNALS || cairo_signal_installed(signo) != cairo_signal_handler) {
		return 0;
	}
	atomic_fetch_or(&pending, BIT(signo));
	cairo_vm_interrupt(0);
	atomic_fetch_or(&kicked, BIT(signo));
	pthread_kill(script_thread, signo);
	return 1;
}

// Raises what is pending on the calling thread, the script's: each signal is
// handled by PHP before pthread_kill() returns.
static void cairo_signals_raise_pending(void)
{
	uint64_t signals = atomic_exchange(&pending, 0);
	for (int signo = 1; signals != 0; signo++, signals >>= 1) {
		if (signals & 1) {
			raising = signo;
			pthread_kill(pthread_self(), signo);
			raising = 0;
		}
	}
}

// Whether the code that runs may be interrupted by a handler:
// Cairo\Internal\Signals::interruptible($file).
static bool cairo_signals_interruptible(void)
{
	zend_string *file = zend_get_executed_filename_ex();
	zval fn, ret, arg;
	ZVAL_STRING(&fn, "Cairo\\Internal\\Signals::interruptible");
	if (file != NULL) {
		ZVAL_STR_COPY(&arg, file);
	} else {
		ZVAL_NULL(&arg);
	}
	ZVAL_UNDEF(&ret);
	bool yes = call_user_function(NULL, NULL, &fn, &ret, 1, &arg) == SUCCESS
		&& EG(exception) == NULL && Z_TYPE(ret) == IS_TRUE;
	zval_ptr_dtor(&fn);
	zval_ptr_dtor(&arg);
	zval_ptr_dtor(&ret);
	return yes;
}

// The thread's interrupt, inside whatever fiber is running. Inside Cairo's
// own code the signal stays pending and Go asks again: an interrupt that
// this one asked for would come before the code has moved on.
static void cairo_signals_interrupt(int tid)
{
	(void)tid;
	if (atomic_load(&pending) != 0 && atomic_load(&scheduled) && cairo_signals_interruptible()) {
		cairo_signals_raise_pending();
	}
}

int cairo_signals_interrupt_again(void)
{
	if (atomic_load(&pending) == 0 || !atomic_load(&scheduled)) {
		return 0;
	}
	cairo_vm_interrupt(0);
	return 1;
}

// Puts cairo_signal_handler() in front of the handler PHP just installed for
// signo. Returns whether Go now receives the signal for the script.
static bool cairo_signal_interpose(int signo)
{
	struct sigaction sa;
	if (go_handlers[signo] == NULL || sigaction(signo, NULL, &sa) != 0 || !(sa.sa_flags & SA_SIGINFO)
		|| sa.sa_handler == SIG_DFL || sa.sa_handler == SIG_IGN) {
		return false;
	}
	if (sa.sa_sigaction == cairo_signal_handler) {
		return true;
	}
	php_handlers[signo] = sa.sa_sigaction;
	sa.sa_sigaction = cairo_signal_handler;
	return sigaction(signo, &sa, NULL) == 0;
}

// Under the scheduler the script's first handler starts the fiber that
// handles signals while no other fiber runs.
static void cairo_signals_start_fiber(void)
{
	if (atomic_load(&scheduled) || cairo_thread_id() < 0 || EG(active_fiber) == NULL) {
		return;
	}
	zval fn, ret;
	ZVAL_STRING(&fn, "Cairo\\Internal\\Signals::watch");
	ZVAL_UNDEF(&ret);
	call_user_function(NULL, NULL, &fn, &ret, 0, NULL);
	zval_ptr_dtor(&fn);
	zval_ptr_dtor(&ret);
}

// pcntl_signal(), which stays PHP's: around it Go begins or ends to listen
// for the signal.
static ZEND_NAMED_FUNCTION(cairo_pcntl_signal)
{
	int signo = 0;
	bool handles = false;
	if (ZEND_NUM_ARGS() >= 2) {
		zval *signal = ZEND_CALL_ARG(execute_data, 1);
		zend_long n = Z_TYPE_P(signal) == IS_LONG || Z_TYPE_P(signal) == IS_STRING ? zval_get_long(signal) : 0;
		signo = n >= 1 && n <= SIGNALS ? (int)n : 0;
		handles = Z_TYPE_P(ZEND_CALL_ARG(execute_data, 2)) != IS_LONG;
	}
	bool handled = signo != 0 && cairo_signal_installed(signo) == cairo_signal_handler;
	if (signo != 0 && handles) {
		// Go listens before PHP's handler is there: no signal falls between the two.
		script_thread = pthread_self();
		goSignalListen(signo);
		if (go_handlers[signo] == NULL) {
			// A signal the process ignored when it started: Go took it just now.
			handler go = cairo_signal_installed(signo);
			go_handlers[signo] = go != cairo_signal_handler ? go : NULL;
		}
	}
	pcntl_signal_original(INTERNAL_FUNCTION_PARAM_PASSTHRU);
	if (signo == 0) {
		return;
	}
	if (handles && Z_TYPE_P(return_value) == IS_TRUE) {
		if (cairo_signal_interpose(signo)) {
			cairo_signals_start_fiber();
			return;
		}
	} else if (handles && handled) {
		return;   // refused: the handler from before stays
	}
	goSignalStop(signo);   // SIG_DFL, SIG_IGN, or no handler after all
}

// cairo_signals_watch(bool $on): void says whether the fiber of
// Cairo\Internal\Signals lives.
PHP_FUNCTION(cairo_signals_watch)
{
	bool on;
	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_BOOL(on)
	ZEND_PARSE_PARAMETERS_END();
	atomic_store(&scheduled, on);
}

// cairo_signals_raise(): void hands PHP the signals that Go received: the
// script's handlers run in the calling fiber, on the way out.
PHP_FUNCTION(cairo_signals_raise)
{
	ZEND_PARSE_PARAMETERS_NONE();
	cairo_signals_raise_pending();
}

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_signals_watch, 0, 0, 1)
	ZEND_ARG_INFO(0, on)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_signals_raise, 0, 0, 0)
ZEND_END_ARG_INFO()

static const zend_function_entry cairo_signals_functions[] = {
	PHP_FE(cairo_signals_watch, arginfo_cairo_signals_watch)
	PHP_FE(cairo_signals_raise, arginfo_cairo_signals_raise)
	PHP_FE_END
};

static void cairo_signals_remove(const char *function)
{
	zend_hash_str_del(CG(function_table), function, strlen(function));
}

// Module startup: Go's handlers are still the ones installed, and no thread
// has copied the function table.
static void cairo_signals_startup(void)
{
	zend_module_entry *pcntl = zend_hash_str_find_ptr(&module_registry, ZEND_STRL("pcntl"));
	if (pcntl == NULL) {
		return;   // a PHP built without it
	}
	if (strcmp(sapi_module.name, "cli") != 0) {
		for (const zend_function_entry *fe = pcntl->functions; fe != NULL && fe->fname != NULL; fe++) {
			cairo_signals_remove(fe->fname);
		}
		return;
	}
	static const char *const left_out[] = {
		"pcntl_fork", "pcntl_rfork", "pcntl_forkx", "pcntl_sigwaitinfo", "pcntl_sigtimedwait",
	};
	for (size_t i = 0; i < sizeof(left_out) / sizeof(left_out[0]); i++) {
		cairo_signals_remove(left_out[i]);
	}
	for (int signo = 1; signo <= SIGNALS; signo++) {
		go_handlers[signo] = cairo_signal_installed(signo);
	}
	zend_function *fn = zend_hash_str_find_ptr(CG(function_table), ZEND_STRL("pcntl_signal"));
	if (fn != NULL && fn->type == ZEND_INTERNAL_FUNCTION) {
		pcntl_signal_original = fn->internal_function.handler;
		fn->internal_function.handler = cairo_pcntl_signal;
	}
}

const cairo_extension cairo_signals = {
	.startup = cairo_signals_startup,
	.interrupt = cairo_signals_interrupt,
	.functions = cairo_signals_functions,
};

// Classic PHP: a PHP request for each request, and the scripts it runs. See
// classic.h.

#include <php.h>
#include <php_main.h>
#include <zend_exceptions.h>
#include <ext/standard/basic_functions.h>

#include "classic.h"
#include "extension.h"
#include "fiber_local.h"
#include "fibers.h"
#include "loop.h"
#include "parked.h"
#include "sockets.h"

static __thread bool again;
static __thread bool checked;

// Every __thread variable of the core or an extension that points into the
// request's memory is reset here: the thread's next PHP request would
// otherwise use freed memory (see AGENTS.md, C).
void cairo_request_ended(void)
{
	cairo_fibers_request_end();
	cairo_local_request_end();
	cairo_parked_request_end();
	cairo_loop_request_end();
	cairo_sockets_request_end();
	cairo_extensions_request_end();
}

bool cairo_classic_again(void)
{
	bool a = again;
	again = false;
	return a;
}

// The thread's request is over and it was not asked to leave: once this PHP
// request has ended, a fresh one takes the thread's next request.
PHP_FUNCTION(cairo_thread_again)
{
	ZEND_PARSE_PARAMETERS_NONE();
	again = true;
}

void cairo_classic_forget(void)
{
	checked = false;
}

PHP_FUNCTION(cairo_thread_checked)
{
	ZEND_PARSE_PARAMETERS_NONE();
	RETURN_BOOL(checked);
}

PHP_FUNCTION(cairo_thread_check_passed)
{
	ZEND_PARSE_PARAMETERS_NONE();
	checked = true;
}

// Runs fn at the bottom of the stack, as PHP runs a request's script and its
// shutdown functions: a variable the code sets outside any function is a
// global, and an exception nobody catches is reported as PHP reports it.
static void at_bottom(void (*fn)(void *), void *arg)
{
	zend_execute_data *caller = EG(current_execute_data);
	EG(current_execute_data) = NULL;
	fn(arg);
	EG(current_execute_data) = caller;
	// exit() is the end of the script, not of the request: what it wrote is
	// the answer.
	if (EG(exception) != NULL && zend_is_unwind_exit(EG(exception))) {
		zend_clear_exception();
	}
}

static void execute(void *fh)
{
	php_execute_script(fh);   // false for exit() too: CG(unclean_shutdown) tells a fatal error
}

static void shutdown_functions(void *arg)
{
	(void)arg;
	php_call_shutdown_functions();
	php_free_shutdown_functions();
}

// cairo_run_script(string $file): bool
// Runs the script $file of a classic request in the calling fiber, as PHP
// runs a request's script: in the global scope, with auto_prepend_file and
// auto_append_file, and then the functions it registered with
// register_shutdown_function(), before the request is answered (as PHP-FPM
// runs them before its output ends). exit() ends the script. Returns false
// when a fatal error ended it, which PHP has reported: the request is then
// answered with what was written, and nothing more runs in this PHP request.
PHP_FUNCTION(cairo_run_script)
{
	zend_string *file;
	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_PATH_STR(file)
	ZEND_PARSE_PARAMETERS_END();
	zend_file_handle fh;
	zend_stream_init_filename_ex(&fh, file);
	fh.primary_script = 1;
	at_bottom(execute, &fh);
	zend_destroy_file_handle(&fh);
	bool fatal = CG(unclean_shutdown);
	// PHP runs them after a fatal error too: WordPress shows its error page
	// from one.
	at_bottom(shutdown_functions, NULL);
	// The status of exit() is a script's, not the thread's.
	EG(exit_status) = 0;
	RETURN_BOOL(!fatal);
}

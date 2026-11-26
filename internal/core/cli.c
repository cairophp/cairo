// PHP's command line on Cairo: `cairo php`.
//
// One thread runs one script the way PHP's own command line does, and this
// file follows it (sapi/cli/php_cli.c): the SAPI goes by "cli", which is what
// PHP itself and libraries ask to know they run in a terminal, output goes
// to standard output as it is written, errors are shown, and the script has
// $argv, STDIN, STDOUT and STDERR. No server runs. The thread is PHP thread
// 0 of the engine, and what it runs is Cairo's runtime, whose scheduler
// takes the script as the thread's one request: the script runs in a fiber
// (cairo_cli_script()), so what it waits for suspends that fiber, and the
// fibers it starts run beside it. A run that is not scheduled has the
// runtime's classes and no scheduler: the script is the thread's own code,
// as under PHP, and every wait is the thread's.

#include <errno.h>
#include <poll.h>
#include <pthread.h>
#include <signal.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <unistd.h>

#include <php.h>
#include <php_main.h>
#include <php_variables.h>
#include <SAPI.h>
#include <TSRM.h>
#include <zend_signal.h>
#include <zend_stream.h>

#include "cli.h"
#include "module.h"
#include "_cgo_export.h"

#define CLI_STDIN_NAME "Standard input code"

// The script as it was named, for $_SERVER.
static const char *cli_self = "";

// Output is not buffered here, as with PHP's command line. A reader that
// went away (`cairo php script.php | head`) ends the script.
static size_t cli_ub_write(const char *str, size_t len)
{
	size_t done = 0;
	while (done < len) {
		ssize_t n = write(STDOUT_FILENO, str + done, len - done);
		if (n > 0) {
			done += (size_t)n;
			continue;
		}
		if (n < 0 && errno == EAGAIN) {
			struct pollfd out = {STDOUT_FILENO, POLLOUT, 0};
			poll(&out, 1, -1);
			continue;
		}
		if (n < 0 && errno == EINTR) {
			continue;
		}
		EG(exit_status) = 255;
		php_handle_aborted_connection();
		break;
	}
	return done;
}

static void cli_flush(void *server_context)
{
}

// header() is accepted and does nothing.
static int cli_header_handler(sapi_header_struct *h, sapi_header_op_enum op, sapi_headers_struct *s)
{
	return 0;
}

static int cli_send_headers(sapi_headers_struct *headers)
{
	return SAPI_HEADER_SENT_SUCCESSFULLY;
}

static void cli_send_header(sapi_header_struct *header, void *server_context)
{
}

static char *cli_read_cookies(void)
{
	return NULL;
}

static void cli_register_variables(zval *track_vars_array)
{
	php_import_environment_variables(track_vars_array);
	php_register_variable("PHP_SELF", cli_self, track_vars_array);
	php_register_variable("SCRIPT_NAME", cli_self, track_vars_array);
	php_register_variable("SCRIPT_FILENAME", cli_self, track_vars_array);
	php_register_variable("PATH_TRANSLATED", cli_self, track_vars_array);
	php_register_variable("DOCUMENT_ROOT", "", track_vars_array);
}

static void cli_log_message(const char *message, int syslog_type)
{
	fprintf(stderr, "%s\n", message);
}

static void cli_ini_defaults(HashTable *configuration_hash)
{
	cairo_shared_ini_defaults(configuration_hash);
	zval on;
	ZVAL_NEW_STR(&on, zend_string_init("1", 1, 1));
	zend_hash_str_update(configuration_hash, "display_errors", sizeof("display_errors") - 1, &on);
}

static int cli_startup(sapi_module_struct *sapi)
{
	return php_module_startup(sapi, &cairo_module_entry);
}

static sapi_module_struct cli_module = {
	"cli",                        /* name */
	"Command Line Interface",     /* pretty name */

	cli_startup,                  /* startup */
	php_module_shutdown_wrapper,  /* shutdown */

	NULL,                         /* activate */
	NULL,                         /* deactivate */

	cli_ub_write,                 /* unbuffered write */
	cli_flush,                    /* flush */
	NULL,                         /* get uid */
	NULL,                         /* getenv */

	php_error,                    /* error handler */

	cli_header_handler,           /* header handler */
	cli_send_headers,             /* send headers handler */
	cli_send_header,              /* send header handler */

	NULL,                         /* read POST data */
	cli_read_cookies,             /* read Cookies */

	cli_register_variables,       /* register server variables */
	cli_log_message,              /* log message */
	NULL,                         /* get request time */
	NULL,                         /* child terminate */

	STANDARD_SAPI_MODULE_PROPERTIES
};

// The constants STDIN, STDOUT and STDERR. The streams leave the descriptors
// open when PHP frees them: code that runs at shutdown still writes there.
static void cli_register_file_handles(void)
{
	static const char *const names[] = {"STDIN", "STDOUT", "STDERR"};
	static const char *const urls[] = {"php://stdin", "php://stdout", "php://stderr"};
	php_stream *streams[3];
	for (int i = 0; i < 3; i++) {
		streams[i] = php_stream_open_wrapper_ex(urls[i], i == 0 ? "rb" : "wb", 0, NULL, NULL);
		if (streams[i] == NULL) {
			while (--i >= 0) {
				php_stream_close(streams[i]);
			}
			return;
		}
		streams[i]->flags |= PHP_STREAM_FLAG_NO_RSCR_DTOR_CLOSE;
	}
	for (int i = 0; i < 3; i++) {
		zend_constant c;
		php_stream_to_zval(streams[i], &c.value);
		ZEND_CONSTANT_SET_FLAGS(&c, CONST_CS, 0);
		c.name = zend_string_init_interned(names[i], strlen(names[i]), 0);
		zend_register_constant(&c);
	}
}

// What cairo_cli_script() runs: the run whose script has not begun, and its
// file. Only the thread of the command line reads and writes them.
static const cairo_cli_options *cli_pending;
static zend_file_handle *cli_file;
static bool cli_running;   // the script has begun and not ended

// cairo_cli_script(): void
// Runs the script of the command line. The runtime calls it once: in the
// fiber that its scheduler made for the script, or outside any when the run
// is not scheduled.
PHP_FUNCTION(cairo_cli_script)
{
	ZEND_PARSE_PARAMETERS_NONE();
	const cairo_cli_options *o = cli_pending;
	if (o == NULL) {
		zend_throw_error(NULL, "cairo_cli_script(): no script waits to run");
		RETURN_THROWS();
	}
	cli_pending = NULL;
	// Cairo's runtime does not count as a file the script included: its
	// first included file is itself, as with PHP.
	zend_hash_clean(&EG(included_files));
	goCLILoaded();
	cli_register_file_handles();
	// The script is the bottom of its stack, as it is under PHP, and not a
	// call of the runtime's: its variables are the globals, and a backtrace
	// ends at it.
	zend_execute_data *caller = EG(current_execute_data);
	EG(current_execute_data) = NULL;
	cli_running = true;
	if (o->mode == CAIRO_CLI_CODE) {
		zend_eval_string_ex(o->code, NULL, "Command line code", 1);
	} else {
		php_execute_script(cli_file);
	}
	cli_running = false;
	EG(current_execute_data) = caller;
	goCLIEnded();
	if (CG(unclean_shutdown)) {
		zend_bailout();   // a fatal error that php_execute_script() caught ends the fiber too
	}
}

// Runs Cairo's runtime, which runs the script.
static void cli_runtime(const char *file)
{
	zend_file_handle fh;
	zend_stream_init_filename(&fh, file);
	zend_execute_scripts(ZEND_REQUIRE, NULL, 1, &fh);
	zend_destroy_file_handle(&fh);
}

// Opens script, or standard input for NULL, as the script of the request.
static int cli_open(zend_file_handle *fh, const char *script)
{
	FILE *fp = stdin;
	if (script != NULL) {
		virtual_cwd_activate();   // a request that ended before this one left it off
		fp = VCWD_FOPEN(script, "rb");
		if (fp == NULL) {
			fprintf(stderr, "Could not open input file: %s\n", script);
			return FAILURE;
		}
	}
	zend_stream_init_fp(fh, fp, script != NULL ? script : CLI_STDIN_NAME);
	fh->primary_script = 1;
	return SUCCESS;
}

// Runs one PHP request for script (NULL: standard input, or no script when
// the mode is CAIRO_CLI_CODE) and returns its exit status.
static int cli_request(const cairo_cli_options *o, const char *script, int argc, char **argv)
{
	zend_file_handle fh;
	bool has_file = o->mode != CAIRO_CLI_CODE;
	char real[MAXPATHLEN];

	if (has_file && cli_open(&fh, script) == FAILURE) {
		return 1;
	}
	cli_self = script != NULL ? script : CLI_STDIN_NAME;
	SG(options) |= SAPI_OPTION_NO_CHDIR;
	SG(request_info).argc = argc;
	SG(request_info).argv = argv;
	SG(request_info).path_translated = script != NULL && VCWD_REALPATH(script, real) ? real : (char *)cli_self;

	if (php_request_startup() == FAILURE) {
		fputs("Could not startup.\n", stderr);
		if (has_file) {
			zend_destroy_file_handle(&fh);
		}
		return 1;
	}
	zend_first_try {
		cairo_signals_onstack();
		CG(skip_shebang) = 1;
		zend_is_auto_global(ZSTR_KNOWN(ZEND_STR_AUTOGLOBAL_SERVER));
		PG(during_request_startup) = 0;

		if (o->mode == CAIRO_CLI_LINT) {
			if (php_lint_script(&fh) == SUCCESS) {
				zend_printf("No syntax errors detected in %s\n", cli_self);
			} else {
				zend_printf("Errors parsing %s\n", cli_self);
				EG(exit_status) = 255;
			}
		} else {
			cli_pending = o;
			cli_file = has_file ? &fh : NULL;
			cli_runtime(o->runtime);
			if (cli_pending != NULL) {
				EG(exit_status) = 255;   // the runtime failed ahead of the script
			}
		}
	} zend_end_try();

	if (cli_running) {
		// The request ends ahead of the script: another fiber called exit(),
		// or nothing could wake the script. PHP unwinds the script's fiber
		// when it destroys it, and would hand what unwinds it, which is no
		// Throwable, to the exception handler the script set.
		zval_ptr_dtor(&EG(user_exception_handler));
		ZVAL_UNDEF(&EG(user_exception_handler));
	}
	if (has_file) {
		zend_destroy_file_handle(&fh);
	}
	php_request_shutdown(NULL);
	SG(request_info).path_translated = NULL;
	return EG(exit_status);
}

static int cli_run(cairo_cli_options *o)
{
	// A socket whose peer went away must not end the script.
	signal(SIGPIPE, SIG_IGN);
#ifdef ZTS
	php_tsrm_startup();
#endif
	zend_signal_startup();
	sapi_startup(&cli_module);   // clears ini_entries; set them after

	cli_module.ini_defaults = cli_ini_defaults;
	cli_module.ini_entries = (char *)o->ini;   // not const before PHP 8.3
	cli_module.php_ini_ignore_cwd = 1;
	cli_module.php_ini_ignore = o->ini_ignore;
	cli_module.php_ini_path_override = (char *)o->ini_path;
	cli_module.phpinfo_as_text = 1;
	cli_module.executable_location = (char *)o->binary;

	if (cli_module.startup(&cli_module) == FAILURE) {
		return 1;
	}
	int status = 0;
	if (o->mode != CAIRO_CLI_LINT) {
		if (o->scheduled) {
			cairo_thread_adopt(0);
		}
		status = cli_request(o, o->mode == CAIRO_CLI_FILE ? o->argv[0] : NULL, o->argc, o->argv);
	} else if (o->argc == 0) {
		status = cli_request(o, NULL, 0, NULL);
	}
	// Each file is checked in a request of its own: two files may declare
	// the same class.
	for (int i = 0; o->mode == CAIRO_CLI_LINT && i < o->argc; i++) {
		if (cli_request(o, o->argv[i], 1, &o->argv[i]) != 0) {
			status = 255;
		}
	}
	php_module_shutdown();
	sapi_shutdown();
#ifdef ZTS
	tsrm_shutdown();
#endif
	return status;
}

static void *cli_thread(void *arg)
{
	return (void *)(intptr_t)cli_run(arg);
}

int cairo_cli(cairo_cli_options *o)
{
	// A thread made here and not by Go, with the stack of a PHP thread: PHP
	// runs a fiber on a stack of its own, and Go accepts calls from such a
	// stack only on threads it did not create (cairo_thread_start).
	pthread_attr_t attr;
	pthread_t thread;
	void *status = NULL;
	pthread_attr_init(&attr);
	pthread_attr_setstacksize(&attr, 16 * 1024 * 1024);
	int rc = pthread_create(&thread, &attr, cli_thread, o);
	pthread_attr_destroy(&attr);
	if (rc != 0 || pthread_join(thread, &status) != 0) {
		return 255;
	}
	return (int)(intptr_t)status;
}

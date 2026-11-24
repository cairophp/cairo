#ifndef CAIRO_CLI_H
#define CAIRO_CLI_H

// What a run of the command line does.
enum {
	CAIRO_CLI_FILE,   // runs the script argv[0]
	CAIRO_CLI_STDIN,  // runs the script it reads from standard input
	CAIRO_CLI_CODE,   // runs code (-r)
	CAIRO_CLI_LINT,   // checks the syntax of the files in argv (-l)
};

// One run of PHP's command line.
typedef struct {
	int mode;
	const char *runtime;   // Cairo's runtime.php
	const char *code;      // CAIRO_CLI_CODE: the code
	int argc;              // $argv: the script as it was named, then its
	char **argv;           // arguments. CAIRO_CLI_LINT: the files to check
	const char *ini;       // php.ini lines that no php.ini overrides
	const char *ini_path;  // a php.ini file or directory (-c), or NULL
	int ini_ignore;        // no php.ini at all (-n)
	const char *binary;    // this executable: PHP_BINARY
	int scheduled;         // the script runs under Cairo's scheduler, on PHP thread 0
} cairo_cli_options;

// Runs it on a thread of its own and returns PHP's exit status.
int cairo_cli(cairo_cli_options *o);

#endif

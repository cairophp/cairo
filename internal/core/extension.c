// The extensions' C side, as the SAPI module sees it: a table that the
// extensions fill before the engine boots (see extension.h).

#include <php.h>

#include "extension.h"

#define CAIRO_MAX_EXTENSIONS 16

static const cairo_extension *extensions[CAIRO_MAX_EXTENSIONS];
static int extension_count;

int cairo_extension_add(const cairo_extension *ext)
{
	if (extension_count == CAIRO_MAX_EXTENSIONS) {
		return -1;
	}
	extensions[extension_count++] = ext;
	return 0;
}

int cairo_extensions_startup(void)
{
	for (int i = 0; i < extension_count; i++) {
		const cairo_extension *ext = extensions[i];
		if (ext->startup != NULL) {
			ext->startup();
		}
		if (ext->functions != NULL
			&& zend_register_functions(NULL, ext->functions, NULL, MODULE_PERSISTENT) == FAILURE) {
			return FAILURE;
		}
	}
	return SUCCESS;
}

void cairo_extensions_request_end(void)
{
	for (int i = 0; i < extension_count; i++) {
		if (extensions[i]->request_end != NULL) {
			extensions[i]->request_end();
		}
	}
}

void cairo_extensions_request_shutdown(void)
{
	for (int i = 0; i < extension_count; i++) {
		if (extensions[i]->request_shutdown != NULL) {
			extensions[i]->request_shutdown();
		}
	}
}

// Stops at the first one that leaves an exception: the next would not start
// with none pending.
void cairo_extensions_interrupt(int tid)
{
	for (int i = 0; i < extension_count && EG(exception) == NULL; i++) {
		if (extensions[i]->interrupt != NULL) {
			extensions[i]->interrupt(tid);
		}
	}
}

bool cairo_extensions_output(const char *str, size_t len)
{
	for (int i = 0; i < extension_count; i++) {
		if (extensions[i]->output != NULL && extensions[i]->output(str, len)) {
			return true;
		}
	}
	return false;
}

void cairo_extensions_flush(void)
{
	for (int i = 0; i < extension_count; i++) {
		if (extensions[i]->flush != NULL) {
			extensions[i]->flush();
		}
	}
}

const cairo_cserver *cairo_extensions_cserver(void)
{
	for (int i = 0; i < extension_count; i++) {
		if (extensions[i]->cserver != NULL) {
			return extensions[i]->cserver;
		}
	}
	return NULL;
}

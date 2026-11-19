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

void cairo_extensions_thread_end(void)
{
	for (int i = 0; i < extension_count; i++) {
		if (extensions[i]->thread_end != NULL) {
			extensions[i]->thread_end();
		}
	}
}

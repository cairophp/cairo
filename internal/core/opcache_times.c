// opcache and the time of a file, for a reload. See opcache_times.h.

#include <php.h>

#include "opcache_times.h"

#if PHP_VERSION_ID >= 80400
// The thread's own value of the setting (ZTS gives each thread a copy of the
// table): what php.ini set, until the thread ends.
static void set_for_thread(const char *name, const char *value)
{
	zend_ini_entry *entry = zend_hash_str_find_ptr(EG(ini_directives), name, strlen(name));
	if (entry == NULL) {
		return;
	}
	zend_string *v = zend_string_init(value, strlen(value), 1);
	if (entry->on_modify != NULL
		&& entry->on_modify(entry, v, entry->mh_arg1, entry->mh_arg2, entry->mh_arg3, ZEND_INI_STAGE_STARTUP) != SUCCESS) {
		zend_string_release_ex(v, 1);
		return;
	}
	if (entry->value != NULL) {
		zend_string_release(entry->value);
	}
	entry->value = v;
}
#endif

void cairo_opcache_thread(void)
{
#if PHP_VERSION_ID >= 80400
	zend_ini_entry *validates = zend_hash_str_find_ptr(EG(ini_directives), ZEND_STRL("opcache.validate_timestamps"));
	if (validates == NULL || validates->value == NULL || zend_ini_parse_bool(validates->value)) {
		return;   // no opcache, or it compares with the disk already
	}
	set_for_thread("opcache.revalidate_freq", "315360000");
	set_for_thread("opcache.validate_timestamps", "1");
#endif
}

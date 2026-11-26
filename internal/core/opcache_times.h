#ifndef CAIRO_OPCACHE_TIMES_H
#define CAIRO_OPCACHE_TIMES_H

// opcache and the time of a file, for a reload (Engine.Reload).
//
// With opcache.validate_timestamps=0 in php.ini, opcache records no time for
// a file it compiles, and a file with none is never compared with the disk
// again: the threads that a reload starts, which turn the check on
// (runtime.php), could not tell the new code from the old and ran the old.
// Where php.ini turns the check off, a thread turns it on with a
// revalidate_freq of ten years before its first PHP request: opcache records
// the time of each file as it compiles it, which it reads anyway
// (file_update_protection), and compares it again only on a thread that a
// reload started, which turns revalidate_freq down to 0.
//
// It sets the thread's own value of both, as PHP-FPM applies a pool's
// php_admin_value: no request end puts php.ini's back, so a request pays
// nothing for it. PHP 8.4 and newer: before, a reload's threads do not use
// opcache at all (runtime.php).

// On a PHP thread (the declare phase's too), after ts_resource() and before
// its first PHP request.
void cairo_opcache_thread(void);

#endif

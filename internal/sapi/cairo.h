#ifndef CAIRO_SAPI_H
#define CAIRO_SAPI_H

int cairo_boot(const char *runtime_file, const char *php_ini);
void cairo_shutdown(void);
int cairo_thread_run(int tid, const char *runtime_file);
int cairo_thread_start(int tid, const char *runtime_file);
void cairo_interrupt(int tid);

#endif

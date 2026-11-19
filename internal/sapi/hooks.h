#ifndef CAIRO_HOOKS_H
#define CAIRO_HOOKS_H

// The runtime hooks; module startup installs them once.
void cairo_hook_functions(void);   // function_hook.c
void cairo_hook_sockets(void);     // socket_hook.c

#endif

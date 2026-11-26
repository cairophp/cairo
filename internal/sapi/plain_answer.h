#ifndef CAIRO_PLAIN_ANSWER_H
#define CAIRO_PLAIN_ANSWER_H

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

// The answer of the script served in C on the calling thread
// (plain_answer.c), for plain_request.c: the SAPI's output callbacks, and
// the answer once the script has ended.

void cairo_plain_answer_begin(int64_t call);
size_t cairo_plain_write(const char *s, size_t len);
void cairo_plain_flush(void);
int cairo_plain_send_headers(void *headers);

// Answers the call with what the script made of it, unless it was
// answered already: a 504 past the deadline.
void cairo_plain_answer(void);

// Answers with status and a short text of Cairo's own, whatever the script
// wrote.
void cairo_plain_answer_text(int status, const char *text);

// As the PHP request shuts down, while its memory is there; and once it
// has ended.
void cairo_plain_answer_free(void);
void cairo_plain_answer_end(void);

// Calls Cairo\Internal\PlainRequest::$method() once the script has
// loaded Cairo's runtime (php/plain_request.php).
void cairo_plain_runtime_call(const char *method);

// Adds fastcgi_finish_request() and headers_send(), at module startup.
void cairo_plain_answer_startup(void);

#endif

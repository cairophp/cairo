#ifndef CAIRO_HTTP_ANSWER_H
#define CAIRO_HTTP_ANSWER_H

#include "extension.h"

// cairo_respond_http(), which php/http_answer.php ends every HTTP request
// with; httpanswer.go adds it to the SAPI module.
extern const cairo_extension cairo_http_answer;

#endif

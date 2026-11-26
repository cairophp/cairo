#ifndef CAIRO_HTTP_ANSWER_H
#define CAIRO_HTTP_ANSWER_H

#include <php.h>
#include <zend_smart_str.h>

#include "extension.h"

// cairo_respond_http(), which php/http_answer.php ends every HTTP request
// with; httpanswer.go adds it to the SAPI module.
extern const cairo_extension cairo_http_answer;

// Appends the status (an int) and the headers (a map of lists of strings; a
// value that is not an array is a list of one) with internal/wire's tags:
// what internal/httpserver's decodeAnswer reads. Returns FAILURE when a
// header value cannot be turned into a string (an exception is then pending).
int cairo_encode_http_answer(zend_long status, HashTable *headers, smart_str *out);

#endif

// Decodes Go's events (internal/wire) into PHP values. Every length is
// checked against what is left of the buffer before it is used, and nesting
// is limited, so a malformed buffer fails instead of reading past its end.

#include <string.h>

#include <php.h>
#include <ext/json/php_json.h>

#include "wire.h"

#define WIRE_MAX_DEPTH 64

typedef struct {
	const unsigned char *p;
	const unsigned char *end;
} wire_reader;

static int read_u32(wire_reader *r, uint32_t *out)
{
	if (r->end - r->p < 4) {
		return FAILURE;
	}
	*out = (uint32_t)r->p[0] | (uint32_t)r->p[1] << 8 | (uint32_t)r->p[2] << 16 | (uint32_t)r->p[3] << 24;
	r->p += 4;
	return SUCCESS;
}

static int read_u64(wire_reader *r, uint64_t *out)
{
	if (r->end - r->p < 8) {
		return FAILURE;
	}
	uint64_t v = 0;
	for (int i = 7; i >= 0; i--) {
		v = v << 8 | r->p[i];
	}
	*out = v;
	r->p += 8;
	return SUCCESS;
}

// A length-prefixed run of bytes, checked to fit in the buffer.
static int read_bytes(wire_reader *r, const char **s, uint32_t *len)
{
	if (read_u32(r, len) == FAILURE || (uint64_t)(r->end - r->p) < *len) {
		return FAILURE;
	}
	*s = (const char *)r->p;
	r->p += *len;
	return SUCCESS;
}

static int decode_value(wire_reader *r, zval *out, int depth)
{
	if (r->p >= r->end) {
		return FAILURE;
	}
	unsigned char tag = *r->p++;
	uint64_t u;
	uint32_t n;
	const char *s;
	switch (tag) {
	case TAG_NULL:
		ZVAL_NULL(out);
		return SUCCESS;
	case TAG_FALSE:
		ZVAL_FALSE(out);
		return SUCCESS;
	case TAG_TRUE:
		ZVAL_TRUE(out);
		return SUCCESS;
	case TAG_INT:
		if (read_u64(r, &u) == FAILURE) {
			return FAILURE;
		}
		ZVAL_LONG(out, (zend_long)(int64_t)u);
		return SUCCESS;
	case TAG_FLOAT: {
		if (read_u64(r, &u) == FAILURE) {
			return FAILURE;
		}
		double d;
		memcpy(&d, &u, sizeof(d));
		ZVAL_DOUBLE(out, d);
		return SUCCESS;
	}
	case TAG_STRING:
		if (read_bytes(r, &s, &n) == FAILURE) {
			return FAILURE;
		}
		ZVAL_STRINGL_FAST(out, s, n);
		return SUCCESS;
	case TAG_JSON: {
		if (read_bytes(r, &s, &n) == FAILURE) {
			return FAILURE;
		}
		// PHP's JSON scanner stops at a NUL byte past the end, so it gets
		// its own terminated copy rather than the middle of the buffer.
		zend_string *json = zend_string_init(s, n, 0);
		int rc = php_json_decode_ex(out, ZSTR_VAL(json), n, PHP_JSON_OBJECT_AS_ARRAY, PHP_JSON_PARSER_DEFAULT_DEPTH);
		zend_string_release(json);
		return rc;
	}
	case TAG_LIST:
	case TAG_MAP:
		if (depth >= WIRE_MAX_DEPTH || read_u32(r, &n) == FAILURE) {
			return FAILURE;
		}
		// Each entry takes at least one byte (two for a list item's tag, five
		// for a key and tag), so a count beyond what is left is malformed.
		if ((uint64_t)n > (uint64_t)(r->end - r->p)) {
			return FAILURE;
		}
		array_init_size(out, n);
		for (uint32_t i = 0; i < n; i++) {
			zval v;
			if (tag == TAG_LIST) {
				if (decode_value(r, &v, depth + 1) == FAILURE) {
					zval_ptr_dtor(out);
					return FAILURE;
				}
				zend_hash_next_index_insert_new(Z_ARRVAL_P(out), &v);
				continue;
			}
			const char *key;
			uint32_t klen;
			if (read_bytes(r, &key, &klen) == FAILURE) {
				zval_ptr_dtor(out);
				return FAILURE;
			}
			if (decode_value(r, &v, depth + 1) == FAILURE) {
				zval_ptr_dtor(out);
				return FAILURE;
			}
			// symtable: numeric keys become integer keys, as json_decode does.
			zend_symtable_str_update(Z_ARRVAL_P(out), key, klen, &v);
		}
		return SUCCESS;
	}
	return FAILURE;
}

int cairo_wire_decode(const char *buf, size_t len, zval *out)
{
	wire_reader r = {(const unsigned char *)buf, (const unsigned char *)buf + len};
	if (decode_value(&r, out, 0) == FAILURE) {
		return FAILURE;
	}
	if (r.p != r.end) {
		zval_ptr_dtor(out);
		return FAILURE;
	}
	return SUCCESS;
}

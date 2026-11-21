// internal/wire's values in C: Go's events decoded into PHP values, and the
// arguments of a Go operation encoded from them. The decoder checks every
// length against what is left of the buffer before it is used, and nesting
// is limited both ways, so a malformed buffer fails instead of reading past
// its end and an array that holds itself fails instead of never ending.

#include <string.h>

#include <php.h>
#include <ext/json/php_json.h>
#include <zend_smart_str.h>

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

static void put_u32(smart_str *out, uint32_t v)
{
	char b[4] = {(char)(v & 0xff), (char)(v >> 8 & 0xff), (char)(v >> 16 & 0xff), (char)(v >> 24 & 0xff)};
	smart_str_appendl(out, b, 4);
}

static void put_u64(smart_str *out, uint64_t v)
{
	put_u32(out, (uint32_t)v);
	put_u32(out, (uint32_t)(v >> 32));
}

static int put_bytes(smart_str *out, const char *s, size_t len)
{
	if (len > UINT32_MAX) {
		return FAILURE;
	}
	put_u32(out, (uint32_t)len);
	smart_str_appendl(out, s, len);
	return SUCCESS;
}

static int encode_value(smart_str *out, zval *v, int depth);

// An array or the properties of a plain object: a list when the keys are
// 0, 1, 2, ... of an array, a map otherwise, with integer keys as text.
static int encode_table(smart_str *out, HashTable *ht, bool list, int depth)
{
	if (depth >= WIRE_MAX_DEPTH) {
		return FAILURE;
	}
	smart_str_appendc(out, list ? TAG_LIST : TAG_MAP);
	// The count is written once the entries are: a property table can hold
	// slots with no value, which are not entries.
	size_t count_at = ZSTR_LEN(out->s);
	put_u32(out, 0);
	uint32_t n = 0;
	zend_ulong index;
	zend_string *key;
	zval *v;
	ZEND_HASH_FOREACH_KEY_VAL_IND(ht, index, key, v) {
		int ok = SUCCESS;
		if (key != NULL) {
			ok = put_bytes(out, ZSTR_VAL(key), ZSTR_LEN(key));
		} else if (!list) {
			char num[ZEND_LTOA_BUF_LEN];
			ok = put_bytes(out, num, (size_t)snprintf(num, sizeof(num), ZEND_LONG_FMT, (zend_long)index));
		}
		if (ok == FAILURE || encode_value(out, v, depth + 1) == FAILURE) {
			return FAILURE;
		}
		n++;
	} ZEND_HASH_FOREACH_END();
	char *count = ZSTR_VAL(out->s) + count_at;
	count[0] = (char)(n & 0xff);
	count[1] = (char)(n >> 8 & 0xff);
	count[2] = (char)(n >> 16 & 0xff);
	count[3] = (char)(n >> 24 & 0xff);
	return SUCCESS;
}

// An object that is not a plain stdClass says itself how it is encoded
// (JsonSerializable, an enum's value, its public properties): PHP's json
// extension knows the rules, and Go reads its text.
static int encode_object(smart_str *out, zval *v)
{
	smart_str json = {0};
	int ok = php_json_encode(&json, v, PHP_JSON_UNESCAPED_SLASHES | PHP_JSON_UNESCAPED_UNICODE);
	if (ok == SUCCESS && !EG(exception) && json.s != NULL) {
		smart_str_appendc(out, TAG_JSON);
		ok = put_bytes(out, ZSTR_VAL(json.s), ZSTR_LEN(json.s));
	} else {
		ok = FAILURE;
	}
	smart_str_free(&json);
	return ok;
}

static int encode_value(smart_str *out, zval *v, int depth)
{
	ZVAL_DEREF(v);
	switch (Z_TYPE_P(v)) {
	case IS_NULL:
		smart_str_appendc(out, TAG_NULL);
		return SUCCESS;
	case IS_FALSE:
		smart_str_appendc(out, TAG_FALSE);
		return SUCCESS;
	case IS_TRUE:
		smart_str_appendc(out, TAG_TRUE);
		return SUCCESS;
	case IS_LONG:
		smart_str_appendc(out, TAG_INT);
		put_u64(out, (uint64_t)Z_LVAL_P(v));
		return SUCCESS;
	case IS_DOUBLE: {
		uint64_t bits;
		memcpy(&bits, &Z_DVAL_P(v), sizeof(bits));
		smart_str_appendc(out, TAG_FLOAT);
		put_u64(out, bits);
		return SUCCESS;
	}
	case IS_STRING:
		smart_str_appendc(out, TAG_STRING);
		return put_bytes(out, Z_STRVAL_P(v), Z_STRLEN_P(v));
	case IS_ARRAY:
		return encode_table(out, Z_ARRVAL_P(v), zend_array_is_list(Z_ARRVAL_P(v)), depth);
	case IS_OBJECT:
		if (Z_OBJCE_P(v) == zend_standard_class_def) {
			return encode_table(out, Z_OBJPROP_P(v), false, depth);
		}
		return encode_object(out, v);
	}
	return FAILURE; // a resource
}

int cairo_wire_encode(HashTable *args, smart_str *out)
{
	if (encode_table(out, args, zend_array_is_list(args), 0) == SUCCESS) {
		return SUCCESS;
	}
	if (!EG(exception)) {
		zend_value_error("the arguments of a Go operation must be null, bool, int, float, string, array or object, nested at most %d deep", WIRE_MAX_DEPTH);
	}
	return FAILURE;
}

// Cairo's PDO driver for PostgreSQL.
//
// PHP code keeps using PDO with "pgsql:" DSNs; the protocol work is done in Go
// by pgx's pgconn (see pgsql.go). Each call is one request to Go: inside a
// Cairo request fiber it goes through Cairo\Internal\Hooks::pg, which
// suspends the fiber while its goroutine talks to the server, so queries on
// one PHP thread overlap. Elsewhere (the declare phase, offload threads,
// Hook::PGSQL off) the thread waits for Go instead. No libpq is needed.
//
// The driver registers as "pgsql" when PHP has no pdo_pgsql of its own.

#include <stdlib.h>
#include <string.h>

#include <php.h>
#include <zend_exceptions.h>
#include <zend_smart_str.h>
#include <ext/json/php_json.h>
#include <ext/standard/base64.h>
#include <ext/pdo/php_pdo.h>
#include <ext/pdo/php_pdo_driver.h>
#include <main/php_memory_streams.h>

#include "pgsql.h"
#include "_cgo_export.h"

void cairo_pg_result_free(cairo_pg_result *r)
{
	if (r == NULL) {
		return;
	}
	for (int i = 0; i < r->ncols; i++) {
		free(r->names[i]);
	}
	for (long i = 0; i < (long)r->nrows * r->ncols; i++) {
		free(r->vals[i]);
	}
	free(r->names);
	free(r->oids);
	free(r->vals);
	free(r->lens);
	free(r);
}

// PostgreSQL type OIDs the driver maps to PHP types.
#define PG_BOOLOID 16
#define PG_BYTEAOID 17
#define PG_INT8OID 20
#define PG_INT2OID 21
#define PG_INT4OID 23
#define PG_OIDOID 26

typedef struct {
	zend_long conn;
	char *errmsg;   // last error, for errorInfo()
} cairo_pg_dbh;

typedef struct {
	cairo_pg_dbh *H;
	zend_string *query;
	cairo_pg_result *result;
	long current_row;
	int nparams;
	zval *params;   // strings (text format) or null
	char *binary;   // per parameter: 1 for LOB (bytea, sent as raw bytes)
} cairo_pg_stmt;

// ---- talking to Go ---------------------------------------------------------

// Sends one request. Returns 1 with *out set to the decoded response array,
// or 0 when an exception is pending (the request's deadline passed).
static int pg_call(zval *req, zval *out)
{
	smart_str json = {0};
	php_json_encode(&json, req, 0);
	smart_str_0(&json);
	ZVAL_UNDEF(out);

	if (EG(active_fiber) != NULL) {
		zval fname, arg, retval;
		ZVAL_STRING(&fname, "Cairo\\Internal\\Hooks::pg");
		ZVAL_STR_COPY(&arg, json.s);
		ZVAL_UNDEF(&retval);
		int rc = call_user_function(NULL, NULL, &fname, &retval, 1, &arg);
		zval_ptr_dtor(&fname);
		zval_ptr_dtor(&arg);
		if (EG(exception) != NULL) {
			zval_ptr_dtor(&retval);
			smart_str_free(&json);
			return 0;
		}
		if (rc == SUCCESS && Z_TYPE(retval) == IS_STRING) {
			php_json_decode_ex(out, Z_STRVAL(retval), Z_STRLEN(retval), PHP_JSON_OBJECT_AS_ARRAY, 64);
		}
		zval_ptr_dtor(&retval);
	}
	if (Z_TYPE_P(out) != IS_ARRAY) {
		zval_ptr_dtor(out);
		char *res = goPgSync(ZSTR_VAL(json.s), (int)ZSTR_LEN(json.s));
		php_json_decode_ex(out, res, strlen(res), PHP_JSON_OBJECT_AS_ARRAY, 64);
		free(res);
	}
	smart_str_free(&json);
	if (Z_TYPE_P(out) != IS_ARRAY) {
		zval_ptr_dtor(out);
		array_init(out);
		add_assoc_bool(out, "ok", 0);
		add_assoc_string(out, "sqlstate", "HY000");
		add_assoc_string(out, "error", "bad response from the pgsql driver");
	}
	return 1;
}

static int resp_ok(zval *resp)
{
	zval *ok = zend_hash_str_find(Z_ARRVAL_P(resp), "ok", 2);
	return ok != NULL && zend_is_true(ok);
}

static zend_long resp_long(zval *resp, const char *key)
{
	zval *v = zend_hash_str_find(Z_ARRVAL_P(resp), key, strlen(key));
	return v != NULL ? zval_get_long(v) : 0;
}

// Records a failed response as the handle's (or statement's) error, so PDO
// raises it according to the error mode.
static void pg_error(pdo_dbh_t *dbh, pdo_stmt_t *stmt, zval *resp)
{
	cairo_pg_dbh *H = dbh->driver_data;
	pdo_error_type *err = stmt != NULL ? &stmt->error_code : &dbh->error_code;
	zval *state = zend_hash_str_find(Z_ARRVAL_P(resp), "sqlstate", 8);
	zval *msg = zend_hash_str_find(Z_ARRVAL_P(resp), "error", 5);
	strncpy(*err, state != NULL && Z_TYPE_P(state) == IS_STRING ? Z_STRVAL_P(state) : "HY000", 5);
	(*err)[5] = '\0';
	if (H != NULL) {
		pefree(H->errmsg, dbh->is_persistent);
		H->errmsg = pestrdup(msg != NULL && Z_TYPE_P(msg) == IS_STRING ? Z_STRVAL_P(msg) : "unknown error", dbh->is_persistent);
	}
}

static void pg_clear_error(pdo_dbh_t *dbh, pdo_stmt_t *stmt)
{
	strcpy(stmt != NULL ? stmt->error_code : dbh->error_code, PDO_ERR_NONE);
}

static void pg_request(zval *req, const char *kind, zend_long conn)
{
	array_init(req);
	add_assoc_string(req, "kind", (char *)kind);
	add_assoc_long(req, "conn", conn);
}

// Runs SQL with the simple protocol (PDO::exec, transactions). Returns the
// affected row count, or -1 on error (recorded on dbh).
static zend_long pg_exec(pdo_dbh_t *dbh, const char *sql, size_t len)
{
	cairo_pg_dbh *H = dbh->driver_data;
	zval req, resp;
	pg_request(&req, "exec", H->conn);
	add_assoc_stringl(&req, "sql", (char *)sql, len);
	int done = pg_call(&req, &resp);
	zval_ptr_dtor(&req);
	if (!done) {
		pg_clear_error(dbh, NULL);
		return -1;
	}
	zend_long out = -1;
	if (resp_ok(&resp)) {
		pg_clear_error(dbh, NULL);
		out = resp_long(&resp, "affected");
	} else {
		pg_error(dbh, NULL, &resp);
	}
	zval_ptr_dtor(&resp);
	return out;
}

// Runs SQL with the extended protocol and text parameters; returns the
// result set, or NULL on error (recorded on dbh or stmt).
static cairo_pg_result *pg_query(pdo_dbh_t *dbh, pdo_stmt_t *stmt, zend_string *sql, zval *params, const char *binary, int nparams)
{
	cairo_pg_dbh *H = dbh->driver_data;
	zval req, resp, list;
	pg_request(&req, "query", H->conn);
	add_assoc_str(&req, "sql", zend_string_copy(sql));
	array_init(&list);
	for (int i = 0; i < nparams; i++) {
		if (params == NULL || Z_TYPE(params[i]) != IS_STRING) {
			add_next_index_null(&list);
		} else {
			add_next_index_str(&list, php_base64_encode((unsigned char *)Z_STRVAL(params[i]), Z_STRLEN(params[i])));
		}
	}
	add_assoc_zval(&req, "params", &list);
	if (binary != NULL) {
		zval bin;
		array_init(&bin);
		for (int i = 0; i < nparams; i++) {
			if (binary[i]) {
				add_next_index_long(&bin, i);
			}
		}
		add_assoc_zval(&req, "binary", &bin);
	}
	int done = pg_call(&req, &resp);
	zval_ptr_dtor(&req);
	if (!done) {
		pg_clear_error(dbh, stmt);
		return NULL;
	}
	cairo_pg_result *r = NULL;
	if (resp_ok(&resp)) {
		r = goPgTake((long long)resp_long(&resp, "result"));
		pg_clear_error(dbh, stmt);
	} else {
		pg_error(dbh, stmt, &resp);
	}
	zval_ptr_dtor(&resp);
	return r;
}

// ---- statements --------------------------------------------------------------

static void stmt_free_params(cairo_pg_stmt *S)
{
	if (S->params != NULL) {
		for (int i = 0; i < S->nparams; i++) {
			zval_ptr_dtor(&S->params[i]);
		}
		efree(S->params);
		S->params = NULL;
	}
	if (S->binary != NULL) {
		efree(S->binary);
		S->binary = NULL;
	}
	S->nparams = 0;
}

static int pg_stmt_dtor(pdo_stmt_t *stmt)
{
	cairo_pg_stmt *S = stmt->driver_data;
	cairo_pg_result_free(S->result);
	stmt_free_params(S);
	if (S->query != NULL) {
		zend_string_release(S->query);
	}
	efree(S);
	stmt->driver_data = NULL;
	return 1;
}

static int pg_stmt_execute(pdo_stmt_t *stmt)
{
	cairo_pg_stmt *S = stmt->driver_data;
	cairo_pg_result_free(S->result);
	S->result = NULL;
	S->current_row = -1;
	S->result = pg_query(stmt->dbh, stmt, S->query, S->params, S->binary, S->nparams);
	if (S->result == NULL) {
		return 0;
	}
	php_pdo_stmt_set_column_count(stmt, S->result->ncols);
	stmt->row_count = S->result->ncols > 0 ? S->result->nrows : (zend_long)S->result->affected;
	return 1;
}

static int pg_stmt_fetch(pdo_stmt_t *stmt, enum pdo_fetch_orientation ori, zend_long offset)
{
	cairo_pg_stmt *S = stmt->driver_data;
	if (S->result == NULL) {
		return 0;
	}
	long row = S->current_row;
	switch (ori) {
		case PDO_FETCH_ORI_NEXT: row++; break;
		case PDO_FETCH_ORI_PRIOR: row--; break;
		case PDO_FETCH_ORI_FIRST: row = 0; break;
		case PDO_FETCH_ORI_LAST: row = S->result->nrows - 1; break;
		case PDO_FETCH_ORI_ABS: row = offset; break;
		case PDO_FETCH_ORI_REL: row += offset; break;
	}
	if (row < 0 || row >= S->result->nrows) {
		if (ori == PDO_FETCH_ORI_NEXT) {
			S->current_row = S->result->nrows;
		}
		return 0;
	}
	S->current_row = row;
	return 1;
}

static int pg_stmt_describe(pdo_stmt_t *stmt, int colno)
{
	cairo_pg_stmt *S = stmt->driver_data;
	if (S->result == NULL || colno >= S->result->ncols) {
		return 0;
	}
	struct pdo_column_data *col = &stmt->columns[colno];
	col->name = zend_string_init(S->result->names[colno], strlen(S->result->names[colno]), 0);
	col->maxlen = SIZE_MAX;
	col->precision = 0;
	return 1;
}

// bytea comes in hex text format ("\x48656c6c6f"); PHP gets the bytes.
static zend_string *bytea_decode(const char *v, int len)
{
	if (len >= 2 && v[0] == '\\' && v[1] == 'x') {
		size_t n = (size_t)(len - 2) / 2;
		zend_string *out = zend_string_alloc(n, 0);
		for (size_t i = 0; i < n; i++) {
			char hex[3] = {v[2 + 2 * i], v[3 + 2 * i], 0};
			ZSTR_VAL(out)[i] = (char)strtol(hex, NULL, 16);
		}
		ZSTR_VAL(out)[n] = '\0';
		return out;
	}
	return zend_string_init(v, len, 0);
}

static int pg_stmt_get_col(pdo_stmt_t *stmt, int colno, zval *result, enum pdo_param_type *type)
{
	cairo_pg_stmt *S = stmt->driver_data;
	cairo_pg_result *r = S->result;
	if (r == NULL || S->current_row < 0 || S->current_row >= r->nrows || colno >= r->ncols) {
		return 0;
	}
	long k = S->current_row * r->ncols + colno;
	const char *v = r->vals[k];
	if (v == NULL) {
		ZVAL_NULL(result);
		return 1;
	}
	switch (r->oids[colno]) {
		case PG_BOOLOID:
			ZVAL_BOOL(result, v[0] == 't');
			break;
		case PG_INT2OID:
		case PG_INT4OID:
		case PG_OIDOID:
#if SIZEOF_ZEND_LONG >= 8
		case PG_INT8OID:
#endif
			ZVAL_LONG(result, ZEND_STRTOL(v, NULL, 10));
			break;
		case PG_BYTEAOID: {
			// Like pdo_pgsql: bytea comes back as a read-only stream
			// (PDO::PARAM_STR in bindColumn() still gives a string).
			zend_string *bytes = bytea_decode(v, r->lens[k]);
			if (type != NULL && *type == PDO_PARAM_STR) {
				ZVAL_STR(result, bytes);
				break;
			}
			php_stream *stm = php_stream_memory_open(TEMP_STREAM_READONLY, bytes);
			zend_string_release(bytes);
			if (stm == NULL) {
				return 0;
			}
			php_stream_to_zval(stm, result);
			break;
		}
		default:
			ZVAL_STRINGL(result, v, r->lens[k]);
	}
	return 1;
}

static int pg_stmt_param_hook(pdo_stmt_t *stmt, struct pdo_bound_param_data *param, enum pdo_param_event event)
{
	cairo_pg_stmt *S = stmt->driver_data;
	if (!param->is_param) {
		return 1;
	}
	switch (event) {
		case PDO_PARAM_EVT_NORMALIZE:
			// PDO rewrote ? and :name to $1, $2...; map names to positions.
			if (param->name != NULL) {
				if (ZSTR_VAL(param->name)[0] == '$') {
					param->paramno = ZEND_STRTOL(ZSTR_VAL(param->name) + 1, NULL, 10) - 1;
				} else {
					zend_string *pos;
					if (stmt->bound_param_map != NULL
						&& (pos = zend_hash_find_ptr(stmt->bound_param_map, param->name)) != NULL) {
						param->paramno = ZEND_STRTOL(ZSTR_VAL(pos) + 1, NULL, 10) - 1;
					} else {
						pdo_raise_impl_error(stmt->dbh, stmt, "HY093", ZSTR_VAL(param->name));
						return 0;
					}
				}
			}
			break;
		case PDO_PARAM_EVT_EXEC_PRE: {
			if (stmt->bound_param_map == NULL || param->paramno < 0) {
				break;
			}
			if (S->params == NULL) {
				// The rewritten query uses $1..$n; the map's values name them
				// (it also holds other keys, so its size is not n).
				int n = 0;
				zend_string *pos;
				ZEND_HASH_FOREACH_PTR(stmt->bound_param_map, pos) {
					if (pos != NULL && ZSTR_LEN(pos) > 1 && ZSTR_VAL(pos)[0] == '$') {
						int k = (int)ZEND_STRTOL(ZSTR_VAL(pos) + 1, NULL, 10);
						if (k > n) {
							n = k;
						}
					}
				} ZEND_HASH_FOREACH_END();
				S->nparams = n;
				S->params = ecalloc(n > 0 ? n : 1, sizeof(zval));
				S->binary = ecalloc(n > 0 ? n : 1, 1);
				for (int i = 0; i < n; i++) {
					ZVAL_NULL(&S->params[i]);
				}
			}
			if (param->paramno >= S->nparams) {
				pdo_raise_impl_error(stmt->dbh, stmt, "HY093", "parameter number out of range");
				return 0;
			}
			zval *dst = &S->params[param->paramno];
			zval_ptr_dtor(dst);
			zval *v = &param->parameter;
			ZVAL_DEREF(v);
			S->binary[param->paramno] = PDO_PARAM_TYPE(param->param_type) == PDO_PARAM_LOB && Z_TYPE_P(v) != IS_NULL;
			if (Z_TYPE_P(v) == IS_NULL || PDO_PARAM_TYPE(param->param_type) == PDO_PARAM_NULL) {
				ZVAL_NULL(dst);
			} else if (Z_TYPE_P(v) == IS_RESOURCE) {
				php_stream *stm;
				php_stream_from_zval_no_verify(stm, v);
				zend_string *data = stm != NULL ? php_stream_copy_to_mem(stm, PHP_STREAM_COPY_ALL, 0) : NULL;
				if (data != NULL) {
					ZVAL_STR(dst, data);
				} else {
					ZVAL_EMPTY_STRING(dst);
				}
			} else if (Z_TYPE_P(v) == IS_FALSE || Z_TYPE_P(v) == IS_TRUE
				|| PDO_PARAM_TYPE(param->param_type) == PDO_PARAM_BOOL) {
				ZVAL_STRING(dst, zend_is_true(v) ? "t" : "f");
			} else {
				ZVAL_STR(dst, zval_get_string(v));
			}
			break;
		}
		case PDO_PARAM_EVT_FREE:
		case PDO_PARAM_EVT_ALLOC:
		case PDO_PARAM_EVT_EXEC_POST:
		case PDO_PARAM_EVT_FETCH_PRE:
		case PDO_PARAM_EVT_FETCH_POST:
			break;
	}
	return 1;
}

static const char *pg_native_type(unsigned int oid)
{
	switch (oid) {
		case PG_BOOLOID: return "bool";
		case PG_BYTEAOID: return "bytea";
		case PG_INT8OID: return "int8";
		case PG_INT2OID: return "int2";
		case PG_INT4OID: return "int4";
		case 25: return "text";
		case 700: return "float4";
		case 701: return "float8";
		case 1043: return "varchar";
		case 1082: return "date";
		case 1114: return "timestamp";
		case 1184: return "timestamptz";
		case 1700: return "numeric";
		case 2950: return "uuid";
		case 114: return "json";
		case 3802: return "jsonb";
	}
	return NULL;
}

static int pg_stmt_get_column_meta(pdo_stmt_t *stmt, zend_long colno, zval *return_value)
{
	cairo_pg_stmt *S = stmt->driver_data;
	if (S->result == NULL || colno < 0 || colno >= S->result->ncols) {
		return FAILURE;
	}
	array_init(return_value);
	unsigned int oid = S->result->oids[colno];
	add_assoc_long(return_value, "pgsql:oid", oid);
	const char *native = pg_native_type(oid);
	if (native != NULL) {
		add_assoc_string(return_value, "native_type", (char *)native);
	}
	add_assoc_long(return_value, "pdo_type",
		oid == PG_BOOLOID ? PDO_PARAM_BOOL
		: (oid == PG_INT2OID || oid == PG_INT4OID || oid == PG_INT8OID) ? PDO_PARAM_INT
		: oid == PG_BYTEAOID ? PDO_PARAM_LOB : PDO_PARAM_STR);
	return SUCCESS;
}

static int pg_stmt_cursor_closer(pdo_stmt_t *stmt)
{
	cairo_pg_stmt *S = stmt->driver_data;
	cairo_pg_result_free(S->result);
	S->result = NULL;
	S->current_row = -1;
	return 1;
}

static const struct pdo_stmt_methods pg_stmt_methods = {
	pg_stmt_dtor,
	pg_stmt_execute,
	pg_stmt_fetch,
	pg_stmt_describe,
	pg_stmt_get_col,
	pg_stmt_param_hook,
	NULL, // set_attribute
	NULL, // get_attribute
	pg_stmt_get_column_meta,
	NULL, // next_rowset
	pg_stmt_cursor_closer,
};

// ---- the connection --------------------------------------------------------

static void pg_close(pdo_dbh_t *dbh)
{
	cairo_pg_dbh *H = dbh->driver_data;
	if (H == NULL) {
		return;
	}
	if (H->conn > 0) {
		zval req;
		pg_request(&req, "close", H->conn);
		smart_str json = {0};
		php_json_encode(&json, &req, 0);
		smart_str_0(&json);
		free(goPgSync(ZSTR_VAL(json.s), (int)ZSTR_LEN(json.s)));   // no I/O wait worth suspending for
		smart_str_free(&json);
		zval_ptr_dtor(&req);
	}
	pefree(H->errmsg, dbh->is_persistent);
	pefree(H, dbh->is_persistent);
	dbh->driver_data = NULL;
}

static bool pg_prepare(pdo_dbh_t *dbh, zend_string *sql, pdo_stmt_t *stmt, zval *driver_options)
{
	cairo_pg_stmt *S = ecalloc(1, sizeof(*S));
	S->H = dbh->driver_data;
	S->current_row = -1;
	stmt->driver_data = S;
	stmt->methods = &pg_stmt_methods;
	stmt->supports_placeholders = PDO_PLACEHOLDER_NAMED;
	stmt->named_rewrite_template = "$%d";

	zend_string *nsql = NULL;
	int ret = pdo_parse_params(stmt, sql, &nsql);
	if (ret == -1) {
		strcpy(dbh->error_code, stmt->error_code);
		return false;
	}
	S->query = ret == 1 ? nsql : zend_string_copy(sql);
	return true;
}

static zend_long pg_do(pdo_dbh_t *dbh, const zend_string *sql)
{
	return pg_exec(dbh, ZSTR_VAL(sql), ZSTR_LEN(sql));
}

static zend_string *pg_quote(pdo_dbh_t *dbh, const zend_string *unquoted, enum pdo_param_type type)
{
	if (type == PDO_PARAM_LOB) {
		static const char hex[] = "0123456789abcdef";
		size_t n = ZSTR_LEN(unquoted);
		zend_string *out = zend_string_safe_alloc(n, 2, sizeof("'\\x'::bytea") - 1, 0);
		char *p = ZSTR_VAL(out);
		memcpy(p, "'\\x", 3);
		p += 3;
		for (size_t i = 0; i < n; i++) {
			unsigned char c = (unsigned char)ZSTR_VAL(unquoted)[i];
			*p++ = hex[c >> 4];
			*p++ = hex[c & 15];
		}
		memcpy(p, "'::bytea", 8);
		p += 8;
		*p = '\0';
		ZSTR_LEN(out) = p - ZSTR_VAL(out);
		return out;
	}
	// standard_conforming_strings is on (the default since PostgreSQL 9.1):
	// only quotes need doubling.
	smart_str s = {0};
	smart_str_appendc(&s, '\'');
	for (size_t i = 0; i < ZSTR_LEN(unquoted); i++) {
		char c = ZSTR_VAL(unquoted)[i];
		if (c == '\0') {
			continue;   // PostgreSQL text cannot hold NUL bytes
		}
		if (c == '\'') {
			smart_str_appendc(&s, '\'');
		}
		smart_str_appendc(&s, c);
	}
	smart_str_appendc(&s, '\'');
	smart_str_0(&s);
	return s.s;
}

static bool pg_begin(pdo_dbh_t *dbh) { return pg_exec(dbh, "BEGIN", 5) >= 0; }
static bool pg_commit(pdo_dbh_t *dbh) { return pg_exec(dbh, "COMMIT", 6) >= 0; }
static bool pg_rollback(pdo_dbh_t *dbh) { return pg_exec(dbh, "ROLLBACK", 8) >= 0; }

static bool pg_set_attribute(pdo_dbh_t *dbh, zend_long attr, zval *val)
{
	switch (attr) {
		case PDO_ATTR_EMULATE_PREPARES:
		case 1000:   // PDO::PGSQL_ATTR_DISABLE_PREPARES
			return true;   // statements always use the extended protocol
	}
	return false;
}

// The connection's status, without I/O: transaction state and server version.
static int pg_status(pdo_dbh_t *dbh, zval *resp)
{
	cairo_pg_dbh *H = dbh->driver_data;
	zval req;
	pg_request(&req, "status", H->conn);
	smart_str json = {0};
	php_json_encode(&json, &req, 0);
	smart_str_0(&json);
	char *res = goPgSync(ZSTR_VAL(json.s), (int)ZSTR_LEN(json.s));
	smart_str_free(&json);
	zval_ptr_dtor(&req);
	php_json_decode_ex(resp, res, strlen(res), PHP_JSON_OBJECT_AS_ARRAY, 8);
	free(res);
	return Z_TYPE_P(resp) == IS_ARRAY && resp_ok(resp);
}

static bool pg_in_transaction(pdo_dbh_t *dbh)
{
	zval resp;
	ZVAL_UNDEF(&resp);
	bool in = false;
	if (pg_status(dbh, &resp)) {
		zval *tx = zend_hash_str_find(Z_ARRVAL(resp), "tx", 2);
		in = tx != NULL && Z_TYPE_P(tx) == IS_STRING && (Z_STRVAL_P(tx)[0] == 'T' || Z_STRVAL_P(tx)[0] == 'E');
	}
	zval_ptr_dtor(&resp);
	return in;
}

static int pg_get_attribute(pdo_dbh_t *dbh, zend_long attr, zval *val)
{
	switch (attr) {
		case PDO_ATTR_CLIENT_VERSION:
			ZVAL_STRING(val, "cairo pgx");
			return 1;
		case PDO_ATTR_SERVER_VERSION: {
			zval resp;
			ZVAL_UNDEF(&resp);
			int ok = pg_status(dbh, &resp);
			zval *v = ok ? zend_hash_str_find(Z_ARRVAL(resp), "server_version", 14) : NULL;
			if (v != NULL && Z_TYPE_P(v) == IS_STRING) {
				ZVAL_COPY(val, v);
			} else {
				ZVAL_NULL(val);
			}
			zval_ptr_dtor(&resp);
			return 1;
		}
		case PDO_ATTR_CONNECTION_STATUS: {
			zval resp;
			ZVAL_UNDEF(&resp);
			ZVAL_STRING(val, pg_status(dbh, &resp) ? "Connection OK" : "Connection bad");
			zval_ptr_dtor(&resp);
			return 1;
		}
	}
	return 0;
}

static zend_string *pg_last_id(pdo_dbh_t *dbh, const zend_string *name)
{
	zend_string *sql = zend_string_init(name ? "SELECT CURRVAL($1)" : "SELECT LASTVAL()", name ? 18 : 16, 0);
	zval param;
	if (name != NULL) {
		ZVAL_STR_COPY(&param, (zend_string *)name);
	}
	cairo_pg_result *r = pg_query(dbh, NULL, sql, name ? &param : NULL, NULL, name ? 1 : 0);
	if (name != NULL) {
		zval_ptr_dtor(&param);
	}
	zend_string_release(sql);
	if (r == NULL) {
		return NULL;
	}
	zend_string *out = NULL;
	if (r->nrows > 0 && r->ncols > 0 && r->vals[0] != NULL) {
		out = zend_string_init(r->vals[0], r->lens[0], 0);
	}
	cairo_pg_result_free(r);
	return out;
}

static void pg_fetch_err(pdo_dbh_t *dbh, pdo_stmt_t *stmt, zval *info)
{
	cairo_pg_dbh *H = dbh->driver_data;
	if (H != NULL && H->errmsg != NULL) {
		add_next_index_long(info, 7);   // PGRES_FATAL_ERROR, as pdo_pgsql reports
		add_next_index_string(info, H->errmsg);
	}
}

static const struct pdo_dbh_methods pg_methods = {
	pg_close,
	pg_prepare,
	pg_do,
	pg_quote,
	pg_begin,
	pg_commit,
	pg_rollback,
	pg_set_attribute,
	pg_last_id,
	pg_fetch_err,
	pg_get_attribute,
	NULL, // check_liveness
	NULL, // get_driver_methods
	NULL, // persistent_shutdown
	pg_in_transaction,
	NULL, // get_gc
};

static int pg_handle_factory(pdo_dbh_t *dbh, zval *driver_options)
{
	cairo_pg_dbh *H = pecalloc(1, sizeof(*H), dbh->is_persistent);
	dbh->driver_data = H;
	dbh->alloc_own_columns = 1;
	dbh->max_escaped_char_length = 2;

	zend_long timeout = 30;
	if (driver_options != NULL) {
		timeout = pdo_attr_lval(driver_options, PDO_ATTR_TIMEOUT, 30);
	}
	zval req, resp;
	array_init(&req);
	add_assoc_string(&req, "kind", "connect");
	add_assoc_stringl(&req, "dsn", (char *)dbh->data_source, dbh->data_source_len);
	if (dbh->username != NULL) {
		add_assoc_string(&req, "user", dbh->username);
	}
	if (dbh->password != NULL) {
		add_assoc_string(&req, "password", dbh->password);
	}
	add_assoc_double(&req, "timeout", (double)timeout);
	int done = pg_call(&req, &resp);
	zval_ptr_dtor(&req);

	dbh->methods = &pg_methods;
	if (!done) {
		return 0;   // the exception is already pending
	}
	if (!resp_ok(&resp)) {
		pg_error(dbh, NULL, &resp);
		zval *msg = zend_hash_str_find(Z_ARRVAL(resp), "error", 5);
		char *text = msg != NULL && Z_TYPE_P(msg) == IS_STRING ? Z_STRVAL_P(msg) : "connection failed";
		pdo_throw_exception(7, text, &dbh->error_code);
		zval_ptr_dtor(&resp);
		return 0;
	}
	H->conn = resp_long(&resp, "conn");
	zval_ptr_dtor(&resp);
	return 1;
}

static const pdo_driver_t cairo_pdo_pgsql_driver = {
	PDO_DRIVER_HEADER(pgsql),
	pg_handle_factory,
};

// Registers the driver unless PHP already has a pgsql one (pdo_pgsql).
void cairo_register_pdo_pgsql(void)
{
	if (zend_hash_str_exists(&module_registry, "pdo", 3)
		&& !zend_hash_str_exists(&module_registry, "pdo_pgsql", 9)) {
		php_pdo_register_driver(&cairo_pdo_pgsql_driver);
	}
}

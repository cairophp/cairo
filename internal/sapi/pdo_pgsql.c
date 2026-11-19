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

#include <errno.h>
#include <stdlib.h>
#include <string.h>

#include <php.h>
#include <zend_exceptions.h>
#include <zend_smart_str.h>
#include <ext/json/php_json.h>
#include <ext/standard/base64.h>
#include <ext/standard/file.h>
#include <ext/pdo/php_pdo.h>
#include <ext/pdo/php_pdo_driver.h>
#include <ext/pdo/php_pdo_error.h>
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
	char tx;        // transaction status after the last reply: I, T or E
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
// or 0 when an exception is pending (the request's deadline passed). Every
// reply from a connection carries its transaction status, kept on H (NULL
// while connecting) so inTransaction() costs no call to Go.
static int pg_call(cairo_pg_dbh *H, zval *req, zval *out)
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
	zval *tx = H != NULL ? zend_hash_str_find(Z_ARRVAL_P(out), "tx", 2) : NULL;
	if (tx != NULL && Z_TYPE_P(tx) == IS_STRING && Z_STRLEN_P(tx) == 1) {
		H->tx = Z_STRVAL_P(tx)[0];
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
	int done = pg_call(dbh->driver_data, &req, &resp);
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
	int done = pg_call(dbh->driver_data, &req, &resp);
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
	cairo_pg_dbh *H = dbh->driver_data;
	return H->tx == 'T' || H->tx == 'E';
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

// ---- PDO::pgsql* methods -----------------------------------------------------
//
// pdo_pgsql's driver-specific methods: COPY in and out, LISTEN/NOTIFY, large
// objects and the backend PID.

static void pg_error_msg(pdo_dbh_t *dbh, const char *state, const char *msg)
{
	zval resp;
	array_init(&resp);
	add_assoc_string(&resp, "sqlstate", (char *)state);
	add_assoc_string(&resp, "error", (char *)msg);
	pg_error(dbh, NULL, &resp);
	zval_ptr_dtor(&resp);
}

// Runs a request that only reports success; 1 on success, 0 on failure (the
// error is recorded on dbh and raised per the error mode, or an exception is
// already pending). *resp is set on success and the caller frees it.
static int pg_method_call(pdo_dbh_t *dbh, zval *req, zval *resp)
{
	int done = pg_call(dbh->driver_data, req, resp);
	zval_ptr_dtor(req);
	if (!done) {
		return 0;
	}
	if (!resp_ok(resp)) {
		pg_error(dbh, NULL, resp);
		zval_ptr_dtor(resp);
		ZVAL_UNDEF(resp);
		PDO_HANDLE_DBH_ERR();
		return 0;
	}
	return 1;
}

static char *pg_copy_sql(zend_string *table, zend_string *fields, bool in, zend_string *sep, zend_string *null_as)
{
	char *sql;
	spprintf(&sql, 0, "COPY %s%s%s%s %s WITH DELIMITER E'%c' NULL AS E'%s'",
		ZSTR_VAL(table),
		fields != NULL ? " (" : "", fields != NULL ? ZSTR_VAL(fields) : "", fields != NULL ? ")" : "",
		in ? "FROM STDIN" : "TO STDOUT",
		sep != NULL && ZSTR_LEN(sep) > 0 ? ZSTR_VAL(sep)[0] : '\t',
		null_as != NULL && ZSTR_LEN(null_as) > 0 ? ZSTR_VAL(null_as) : "\\\\N");
	return sql;
}

// Sends COPY data (rows already terminated by newlines) into a table.
static bool pg_copy_in(pdo_dbh_t *dbh, char *sql, zend_string *data)
{
	cairo_pg_dbh *H = dbh->driver_data;
	zval req, resp;
	pg_request(&req, "copy_in", H->conn);
	add_assoc_string(&req, "sql", sql);
	add_assoc_str(&req, "data", php_base64_encode((unsigned char *)ZSTR_VAL(data), ZSTR_LEN(data)));
	efree(sql);
	if (!pg_method_call(dbh, &req, &resp)) {
		return false;
	}
	zval_ptr_dtor(&resp);
	return true;
}

// Reads a table as COPY text; NULL on failure.
static zend_string *pg_copy_out(pdo_dbh_t *dbh, char *sql)
{
	cairo_pg_dbh *H = dbh->driver_data;
	zval req, resp;
	pg_request(&req, "copy_out", H->conn);
	add_assoc_string(&req, "sql", sql);
	efree(sql);
	if (!pg_method_call(dbh, &req, &resp)) {
		return NULL;
	}
	zval *data = zend_hash_str_find(Z_ARRVAL(resp), "data", 4);
	zend_string *out = NULL;
	if (data != NULL && Z_TYPE_P(data) == IS_STRING) {
		out = php_base64_decode((unsigned char *)Z_STRVAL_P(data), Z_STRLEN_P(data));
	}
	zval_ptr_dtor(&resp);
	if (out == NULL) {
		out = ZSTR_EMPTY_ALLOC();
	}
	return out;
}

static ZEND_NAMED_FUNCTION(pg_copy_from_array)
{
	zend_string *table, *sep = NULL, *null_as = NULL, *fields = NULL;
	zval *rows, *row;
	ZEND_PARSE_PARAMETERS_START(2, 5)
		Z_PARAM_STR(table)
		Z_PARAM_ARRAY(rows)
		Z_PARAM_OPTIONAL
		Z_PARAM_STR(sep)
		Z_PARAM_STR(null_as)
		Z_PARAM_STR_OR_NULL(fields)
	ZEND_PARSE_PARAMETERS_END();
	if (zend_hash_num_elements(Z_ARRVAL_P(rows)) == 0) {
		zend_argument_value_error(2, "cannot be empty");
		RETURN_THROWS();
	}
	pdo_dbh_t *dbh = Z_PDO_DBH_P(ZEND_THIS);
	PDO_CONSTRUCT_CHECK;
	PDO_DBH_CLEAR_ERR();

	smart_str buf = {0};
	ZEND_HASH_FOREACH_VAL(Z_ARRVAL_P(rows), row) {
		zend_string *line = zval_try_get_string(row);
		if (line == NULL) {
			smart_str_free(&buf);
			RETURN_THROWS();
		}
		smart_str_append(&buf, line);
		if (ZSTR_LEN(line) == 0 || ZSTR_VAL(line)[ZSTR_LEN(line) - 1] != '\n') {
			smart_str_appendc(&buf, '\n');
		}
		zend_string_release(line);
	} ZEND_HASH_FOREACH_END();
	smart_str_0(&buf);
	bool ok = pg_copy_in(dbh, pg_copy_sql(table, fields, true, sep, null_as), buf.s);
	smart_str_free(&buf);
	if (EG(exception) != NULL) {
		RETURN_THROWS();
	}
	RETURN_BOOL(ok);
}

static ZEND_NAMED_FUNCTION(pg_copy_from_file)
{
	zend_string *table, *filename, *sep = NULL, *null_as = NULL, *fields = NULL;
	ZEND_PARSE_PARAMETERS_START(2, 5)
		Z_PARAM_STR(table)
		Z_PARAM_PATH_STR(filename)
		Z_PARAM_OPTIONAL
		Z_PARAM_STR(sep)
		Z_PARAM_STR(null_as)
		Z_PARAM_STR_OR_NULL(fields)
	ZEND_PARSE_PARAMETERS_END();
	pdo_dbh_t *dbh = Z_PDO_DBH_P(ZEND_THIS);
	PDO_CONSTRUCT_CHECK;
	PDO_DBH_CLEAR_ERR();

	php_stream *stream = php_stream_open_wrapper_ex(ZSTR_VAL(filename), "rb", 0, NULL, FG(default_context));
	if (stream == NULL) {
		pg_error_msg(dbh, "HY000", "Unable to open the file");
		PDO_HANDLE_DBH_ERR();
		RETURN_FALSE;
	}
	zend_string *data = php_stream_copy_to_mem(stream, PHP_STREAM_COPY_ALL, 0);
	php_stream_close(stream);
	if (data == NULL) {
		data = ZSTR_EMPTY_ALLOC();
	}
	bool ok = pg_copy_in(dbh, pg_copy_sql(table, fields, true, sep, null_as), data);
	zend_string_release(data);
	if (EG(exception) != NULL) {
		RETURN_THROWS();
	}
	RETURN_BOOL(ok);
}

static ZEND_NAMED_FUNCTION(pg_copy_to_array)
{
	zend_string *table, *sep = NULL, *null_as = NULL, *fields = NULL;
	ZEND_PARSE_PARAMETERS_START(1, 4)
		Z_PARAM_STR(table)
		Z_PARAM_OPTIONAL
		Z_PARAM_STR(sep)
		Z_PARAM_STR(null_as)
		Z_PARAM_STR_OR_NULL(fields)
	ZEND_PARSE_PARAMETERS_END();
	pdo_dbh_t *dbh = Z_PDO_DBH_P(ZEND_THIS);
	PDO_CONSTRUCT_CHECK;
	PDO_DBH_CLEAR_ERR();

	zend_string *data = pg_copy_out(dbh, pg_copy_sql(table, fields, false, sep, null_as));
	if (data == NULL) {
		if (EG(exception) != NULL) {
			RETURN_THROWS();
		}
		RETURN_FALSE;
	}
	array_init(return_value);
	const char *p = ZSTR_VAL(data), *end = p + ZSTR_LEN(data);
	while (p < end) {
		const char *nl = memchr(p, '\n', end - p);
		const char *stop = nl != NULL ? nl + 1 : end;
		add_next_index_stringl(return_value, p, stop - p);
		p = stop;
	}
	zend_string_release(data);
}

static ZEND_NAMED_FUNCTION(pg_copy_to_file)
{
	zend_string *table, *filename, *sep = NULL, *null_as = NULL, *fields = NULL;
	ZEND_PARSE_PARAMETERS_START(2, 5)
		Z_PARAM_STR(table)
		Z_PARAM_PATH_STR(filename)
		Z_PARAM_OPTIONAL
		Z_PARAM_STR(sep)
		Z_PARAM_STR(null_as)
		Z_PARAM_STR_OR_NULL(fields)
	ZEND_PARSE_PARAMETERS_END();
	pdo_dbh_t *dbh = Z_PDO_DBH_P(ZEND_THIS);
	PDO_CONSTRUCT_CHECK;
	PDO_DBH_CLEAR_ERR();

	php_stream *stream = php_stream_open_wrapper_ex(ZSTR_VAL(filename), "wb", 0, NULL, FG(default_context));
	if (stream == NULL) {
		pg_error_msg(dbh, "HY000", "Unable to open the file for writing");
		PDO_HANDLE_DBH_ERR();
		RETURN_FALSE;
	}
	zend_string *data = pg_copy_out(dbh, pg_copy_sql(table, fields, false, sep, null_as));
	if (data == NULL) {
		php_stream_close(stream);
		if (EG(exception) != NULL) {
			RETURN_THROWS();
		}
		RETURN_FALSE;
	}
	bool ok = php_stream_write(stream, ZSTR_VAL(data), ZSTR_LEN(data)) == (ssize_t)ZSTR_LEN(data);
	php_stream_close(stream);
	zend_string_release(data);
	if (!ok) {
		pg_error_msg(dbh, "HY000", "Unable to write to the file");
		PDO_HANDLE_DBH_ERR();
	}
	RETURN_BOOL(ok);
}

static ZEND_NAMED_FUNCTION(pg_get_notify)
{
	zend_long mode = PDO_FETCH_USE_DEFAULT, wait_ms = 0;
	ZEND_PARSE_PARAMETERS_START(0, 2)
		Z_PARAM_OPTIONAL
		Z_PARAM_LONG(mode)
		Z_PARAM_LONG(wait_ms)
	ZEND_PARSE_PARAMETERS_END();
	pdo_dbh_t *dbh = Z_PDO_DBH_P(ZEND_THIS);
	PDO_CONSTRUCT_CHECK;
	if (mode == PDO_FETCH_USE_DEFAULT) {
		mode = dbh->default_fetch_type;
	}
	if (mode != PDO_FETCH_BOTH && mode != PDO_FETCH_ASSOC && mode != PDO_FETCH_NUM) {
		zend_argument_value_error(1, "must be one of PDO::FETCH_BOTH, PDO::FETCH_ASSOC, or PDO::FETCH_NUM");
		RETURN_THROWS();
	}
	if (wait_ms < 0) {
		zend_argument_value_error(2, "must be greater than or equal to 0");
		RETURN_THROWS();
	}
	PDO_DBH_CLEAR_ERR();

	cairo_pg_dbh *H = dbh->driver_data;
	zval req, resp;
	pg_request(&req, "notify", H->conn);
	add_assoc_long(&req, "wait_ms", wait_ms);
	if (!pg_method_call(dbh, &req, &resp)) {
		if (EG(exception) != NULL) {
			RETURN_THROWS();
		}
		RETURN_FALSE;
	}
	zval *n = zend_hash_str_find(Z_ARRVAL(resp), "notification", 12);
	if (n == NULL || Z_TYPE_P(n) != IS_ARRAY) {
		zval_ptr_dtor(&resp);
		RETURN_FALSE;
	}
	zval *channel = zend_hash_str_find(Z_ARRVAL_P(n), "channel", 7);
	zend_long pid = resp_long(n, "pid");
	zval *payload = zend_hash_str_find(Z_ARRVAL_P(n), "payload", 7);
	array_init(return_value);
	zend_string *ch = channel != NULL ? zval_get_string(channel) : ZSTR_EMPTY_ALLOC();
	if (mode != PDO_FETCH_ASSOC) {
		add_index_str(return_value, 0, zend_string_copy(ch));
	}
	if (mode != PDO_FETCH_NUM) {
		add_assoc_str(return_value, "message", zend_string_copy(ch));
	}
	zend_string_release(ch);
	if (mode != PDO_FETCH_ASSOC) {
		add_index_long(return_value, 1, pid);
	}
	if (mode != PDO_FETCH_NUM) {
		add_assoc_long(return_value, "pid", pid);
	}
	if (payload != NULL && Z_TYPE_P(payload) == IS_STRING && Z_STRLEN_P(payload) > 0) {
		if (mode != PDO_FETCH_ASSOC) {
			add_index_stringl(return_value, 2, Z_STRVAL_P(payload), Z_STRLEN_P(payload));
		}
		if (mode != PDO_FETCH_NUM) {
			add_assoc_stringl(return_value, "payload", Z_STRVAL_P(payload), Z_STRLEN_P(payload));
		}
	}
	zval_ptr_dtor(&resp);
}

static ZEND_NAMED_FUNCTION(pg_get_pid)
{
	ZEND_PARSE_PARAMETERS_NONE();
	pdo_dbh_t *dbh = Z_PDO_DBH_P(ZEND_THIS);
	PDO_CONSTRUCT_CHECK;
	zval resp;
	ZVAL_UNDEF(&resp);
	zend_long pid = pg_status(dbh, &resp) ? resp_long(&resp, "pid") : 0;
	zval_ptr_dtor(&resp);
	RETURN_LONG(pid);
}

// ---- Large objects -------------------------------------------------------------
//
// pgsqlLOBCreate(), pgsqlLOBOpen() and pgsqlLOBUnlink(), as in pdo_pgsql.
// Each operation is one statement on the server's lo_* functions (Go runs
// it, so in a fiber only the fiber waits). As with pdo_pgsql, large objects
// need a transaction: a descriptor lasts until the transaction ends.

#define CAIRO_INV_WRITE 0x00020000
#define CAIRO_INV_READ  0x00040000

// Sends one "lo" request; 1 with *resp set on success. On failure the error
// is recorded on dbh (raise it with PDO_HANDLE_DBH_ERR, or warn) and 0 is
// returned; an exception may be pending.
static int pg_lo_call(pdo_dbh_t *dbh, zval *req, zval *resp)
{
	int done = pg_call(dbh->driver_data, req, resp);
	zval_ptr_dtor(req);
	if (!done) {
		return 0;
	}
	if (!resp_ok(resp)) {
		pg_error(dbh, NULL, resp);
		zval_ptr_dtor(resp);
		ZVAL_UNDEF(resp);
		return 0;
	}
	return 1;
}

static void pg_lo_request(pdo_dbh_t *dbh, zval *req, const char *op)
{
	cairo_pg_dbh *H = dbh->driver_data;
	pg_request(req, "lo", H->conn);
	add_assoc_string(req, "lo", (char *)op);
}

static bool pg_parse_oid(zend_string *s, zend_long *oid)
{
	char *end;
	errno = 0;
	unsigned long long v = strtoull(ZSTR_VAL(s), &end, 10);
	if (ZSTR_LEN(s) == 0 || *end != '\0' || errno != 0 || v == 0 || v > 0xFFFFFFFFULL) {
		return false;
	}
	*oid = (zend_long)v;
	return true;
}

typedef struct {
	zval dbh;        // keeps the PDO object alive while the stream is open
	zend_long fd;
} cairo_pg_lob;

static void pg_lob_warn(pdo_dbh_t *dbh)
{
	cairo_pg_dbh *H = dbh->driver_data;
	if (EG(exception) == NULL) {
		php_error_docref(NULL, E_WARNING, "Large object: %s", H != NULL && H->errmsg != NULL ? H->errmsg : "failed");
	}
}

static ssize_t pg_lob_write(php_stream *stream, const char *buf, size_t count)
{
	cairo_pg_lob *L = stream->abstract;
	pdo_dbh_t *dbh = Z_PDO_DBH_P(&L->dbh);
	zval req, resp;
	pg_lo_request(dbh, &req, "write");
	add_assoc_long(&req, "fd", L->fd);
	add_assoc_str(&req, "data", php_base64_encode((const unsigned char *)buf, count));
	if (!pg_lo_call(dbh, &req, &resp)) {
		pg_lob_warn(dbh);
		return -1;
	}
	zend_long n = resp_long(&resp, "value");
	zval_ptr_dtor(&resp);
	return n < 0 ? -1 : (ssize_t)n;
}

static ssize_t pg_lob_read(php_stream *stream, char *buf, size_t count)
{
	cairo_pg_lob *L = stream->abstract;
	pdo_dbh_t *dbh = Z_PDO_DBH_P(&L->dbh);
	if (count > INT32_MAX) {
		count = INT32_MAX;
	}
	zval req, resp;
	pg_lo_request(dbh, &req, "read");
	add_assoc_long(&req, "fd", L->fd);
	add_assoc_long(&req, "n", (zend_long)count);
	if (!pg_lo_call(dbh, &req, &resp)) {
		pg_lob_warn(dbh);
		return -1;
	}
	zval *data = zend_hash_str_find(Z_ARRVAL(resp), "data", 4);
	zend_string *bytes = data != NULL && Z_TYPE_P(data) == IS_STRING
		? php_base64_decode((unsigned char *)Z_STRVAL_P(data), Z_STRLEN_P(data)) : NULL;
	zval_ptr_dtor(&resp);
	size_t n = bytes != NULL ? MIN(ZSTR_LEN(bytes), count) : 0;
	if (n > 0) {
		memcpy(buf, ZSTR_VAL(bytes), n);
	}
	if (bytes != NULL) {
		zend_string_release(bytes);
	}
	if (n < count) {
		stream->eof = 1;   // loread returns less only at the end of the object
	}
	return (ssize_t)n;
}

static int pg_lob_close(php_stream *stream, int close_handle)
{
	cairo_pg_lob *L = stream->abstract;
	pdo_dbh_t *dbh = Z_PDO_DBH_P(&L->dbh);
	if (close_handle && dbh->driver_data != NULL && EG(exception) == NULL) {
		zval req, resp;
		pg_lo_request(dbh, &req, "close");
		add_assoc_long(&req, "fd", L->fd);
		if (pg_lo_call(dbh, &req, &resp)) {
			zval_ptr_dtor(&resp);
		}
	}
	zval_ptr_dtor(&L->dbh);
	efree(L);
	return 0;
}

static int pg_lob_flush(php_stream *stream)
{
	return 0;
}

static int pg_lob_seek(php_stream *stream, zend_off_t offset, int whence, zend_off_t *newoffset)
{
	cairo_pg_lob *L = stream->abstract;
	pdo_dbh_t *dbh = Z_PDO_DBH_P(&L->dbh);
	zval req, resp;
	pg_lo_request(dbh, &req, "seek");
	add_assoc_long(&req, "fd", L->fd);
	add_assoc_long(&req, "offset", (zend_long)offset);
	add_assoc_long(&req, "whence", whence);
	if (!pg_lo_call(dbh, &req, &resp)) {
		pg_lob_warn(dbh);
		return -1;
	}
	zend_long pos = resp_long(&resp, "value");
	zval_ptr_dtor(&resp);
	*newoffset = (zend_off_t)pos;
	return pos >= 0 ? 0 : -1;
}

static const php_stream_ops pg_lob_ops = {
	pg_lob_write,
	pg_lob_read,
	pg_lob_close,
	pg_lob_flush,
	"pdo_pgsql lob stream",
	pg_lob_seek,
	NULL, // cast
	NULL, // stat
	NULL, // set_option
};

static ZEND_NAMED_FUNCTION(pg_lob_create)
{
	ZEND_PARSE_PARAMETERS_NONE();
	pdo_dbh_t *dbh = Z_PDO_DBH_P(ZEND_THIS);
	PDO_CONSTRUCT_CHECK;
	PDO_DBH_CLEAR_ERR();
	zval req, resp;
	pg_lo_request(dbh, &req, "create");
	if (!pg_lo_call(dbh, &req, &resp)) {
		if (EG(exception) == NULL) {
			PDO_HANDLE_DBH_ERR();
		}
		if (EG(exception) != NULL) {
			RETURN_THROWS();
		}
		RETURN_FALSE;
	}
	zend_long oid = resp_long(&resp, "value");
	zval_ptr_dtor(&resp);
	RETURN_STR(zend_long_to_str(oid));
}

static ZEND_NAMED_FUNCTION(pg_lob_open)
{
	zend_string *oid_str, *mode = NULL;
	ZEND_PARSE_PARAMETERS_START(1, 2)
		Z_PARAM_STR(oid_str)
		Z_PARAM_OPTIONAL
		Z_PARAM_STR(mode)
	ZEND_PARSE_PARAMETERS_END();
	pdo_dbh_t *dbh = Z_PDO_DBH_P(ZEND_THIS);
	PDO_CONSTRUCT_CHECK;
	PDO_DBH_CLEAR_ERR();
	zend_long oid;
	if (!pg_parse_oid(oid_str, &oid)) {
		pg_error_msg(dbh, "22P02", "Invalid OID value");
		PDO_HANDLE_DBH_ERR();
		if (EG(exception) != NULL) {
			RETURN_THROWS();
		}
		RETURN_FALSE;
	}
	const char *m = mode != NULL ? ZSTR_VAL(mode) : "rb";
	zend_long inv = strpbrk(m, "+w") != NULL ? (CAIRO_INV_READ | CAIRO_INV_WRITE) : CAIRO_INV_READ;
	zval req, resp;
	pg_lo_request(dbh, &req, "open");
	add_assoc_long(&req, "oid", oid);
	add_assoc_long(&req, "mode", inv);
	if (!pg_lo_call(dbh, &req, &resp)) {
		if (EG(exception) == NULL) {
			PDO_HANDLE_DBH_ERR();
		}
		if (EG(exception) != NULL) {
			RETURN_THROWS();
		}
		RETURN_FALSE;
	}
	zend_long fd = resp_long(&resp, "value");
	zval_ptr_dtor(&resp);

	cairo_pg_lob *L = ecalloc(1, sizeof(*L));
	ZVAL_OBJ_COPY(&L->dbh, Z_OBJ_P(ZEND_THIS));
	L->fd = fd;
	php_stream *stream = php_stream_alloc(&pg_lob_ops, L, NULL, m);
	if (stream == NULL) {
		zval_ptr_dtor(&L->dbh);
		efree(L);
		RETURN_FALSE;
	}
	php_stream_to_zval(stream, return_value);
}

static ZEND_NAMED_FUNCTION(pg_lob_unlink)
{
	zend_string *oid_str;
	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_STR(oid_str)
	ZEND_PARSE_PARAMETERS_END();
	pdo_dbh_t *dbh = Z_PDO_DBH_P(ZEND_THIS);
	PDO_CONSTRUCT_CHECK;
	PDO_DBH_CLEAR_ERR();
	zend_long oid;
	if (!pg_parse_oid(oid_str, &oid)) {
		pg_error_msg(dbh, "22P02", "Invalid OID value");
		PDO_HANDLE_DBH_ERR();
		if (EG(exception) != NULL) {
			RETURN_THROWS();
		}
		RETURN_FALSE;
	}
	zval req, resp;
	pg_lo_request(dbh, &req, "unlink");
	add_assoc_long(&req, "oid", oid);
	if (!pg_lo_call(dbh, &req, &resp)) {
		if (EG(exception) == NULL) {
			PDO_HANDLE_DBH_ERR();
		}
		if (EG(exception) != NULL) {
			RETURN_THROWS();
		}
		RETURN_FALSE;
	}
	zval_ptr_dtor(&resp);
	RETURN_TRUE;
}

ZEND_BEGIN_ARG_WITH_RETURN_TYPE_MASK_EX(arginfo_pg_lob_create, 0, 0, MAY_BE_STRING|MAY_BE_FALSE)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_pg_lob_open, 0, 0, 1)
	ZEND_ARG_TYPE_INFO(0, oid, IS_STRING, 0)
	ZEND_ARG_TYPE_INFO_WITH_DEFAULT_VALUE(0, mode, IS_STRING, 0, "\"rb\"")
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_WITH_RETURN_TYPE_INFO_EX(arginfo_pg_lob_unlink, 0, 1, _IS_BOOL, 0)
	ZEND_ARG_TYPE_INFO(0, oid, IS_STRING, 0)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_WITH_RETURN_TYPE_INFO_EX(arginfo_pg_copy_from_array, 0, 2, _IS_BOOL, 0)
	ZEND_ARG_TYPE_INFO(0, tableName, IS_STRING, 0)
	ZEND_ARG_TYPE_INFO(0, rows, IS_ARRAY, 0)
	ZEND_ARG_TYPE_INFO_WITH_DEFAULT_VALUE(0, separator, IS_STRING, 0, "\"\\t\"")
	ZEND_ARG_TYPE_INFO_WITH_DEFAULT_VALUE(0, nullAs, IS_STRING, 0, "\"\\\\\\\\N\"")
	ZEND_ARG_TYPE_INFO_WITH_DEFAULT_VALUE(0, fields, IS_STRING, 1, "null")
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_WITH_RETURN_TYPE_INFO_EX(arginfo_pg_copy_file, 0, 2, _IS_BOOL, 0)
	ZEND_ARG_TYPE_INFO(0, tableName, IS_STRING, 0)
	ZEND_ARG_TYPE_INFO(0, filename, IS_STRING, 0)
	ZEND_ARG_TYPE_INFO_WITH_DEFAULT_VALUE(0, separator, IS_STRING, 0, "\"\\t\"")
	ZEND_ARG_TYPE_INFO_WITH_DEFAULT_VALUE(0, nullAs, IS_STRING, 0, "\"\\\\\\\\N\"")
	ZEND_ARG_TYPE_INFO_WITH_DEFAULT_VALUE(0, fields, IS_STRING, 1, "null")
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_WITH_RETURN_TYPE_MASK_EX(arginfo_pg_copy_to_array, 0, 1, MAY_BE_ARRAY|MAY_BE_FALSE)
	ZEND_ARG_TYPE_INFO(0, tableName, IS_STRING, 0)
	ZEND_ARG_TYPE_INFO_WITH_DEFAULT_VALUE(0, separator, IS_STRING, 0, "\"\\t\"")
	ZEND_ARG_TYPE_INFO_WITH_DEFAULT_VALUE(0, nullAs, IS_STRING, 0, "\"\\\\\\\\N\"")
	ZEND_ARG_TYPE_INFO_WITH_DEFAULT_VALUE(0, fields, IS_STRING, 1, "null")
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_WITH_RETURN_TYPE_MASK_EX(arginfo_pg_get_notify, 0, 0, MAY_BE_ARRAY|MAY_BE_FALSE)
	ZEND_ARG_TYPE_INFO_WITH_DEFAULT_VALUE(0, fetchMode, IS_LONG, 0, "PDO::FETCH_DEFAULT")
	ZEND_ARG_TYPE_INFO_WITH_DEFAULT_VALUE(0, timeoutMilliseconds, IS_LONG, 0, "0")
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_WITH_RETURN_TYPE_INFO_EX(arginfo_pg_get_pid, 0, 0, IS_LONG, 0)
ZEND_END_ARG_INFO()

static const zend_function_entry pg_dbh_functions[] = {
	ZEND_FENTRY(pgsqlCopyFromArray, pg_copy_from_array, arginfo_pg_copy_from_array, ZEND_ACC_PUBLIC)
	ZEND_FENTRY(pgsqlCopyFromFile, pg_copy_from_file, arginfo_pg_copy_file, ZEND_ACC_PUBLIC)
	ZEND_FENTRY(pgsqlCopyToArray, pg_copy_to_array, arginfo_pg_copy_to_array, ZEND_ACC_PUBLIC)
	ZEND_FENTRY(pgsqlCopyToFile, pg_copy_to_file, arginfo_pg_copy_file, ZEND_ACC_PUBLIC)
	ZEND_FENTRY(pgsqlGetNotify, pg_get_notify, arginfo_pg_get_notify, ZEND_ACC_PUBLIC)
	ZEND_FENTRY(pgsqlGetPid, pg_get_pid, arginfo_pg_get_pid, ZEND_ACC_PUBLIC)
	ZEND_FENTRY(pgsqlLOBCreate, pg_lob_create, arginfo_pg_lob_create, ZEND_ACC_PUBLIC)
	ZEND_FENTRY(pgsqlLOBOpen, pg_lob_open, arginfo_pg_lob_open, ZEND_ACC_PUBLIC)
	ZEND_FENTRY(pgsqlLOBUnlink, pg_lob_unlink, arginfo_pg_lob_unlink, ZEND_ACC_PUBLIC)
	ZEND_FE_END
};

static const zend_function_entry *pg_get_driver_methods(pdo_dbh_t *dbh, int kind)
{
	return kind == PDO_DBH_DRIVER_METHOD_KIND_DBH ? pg_dbh_functions : NULL;
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
	pg_get_driver_methods,
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
	int done = pg_call(dbh->driver_data, &req, &resp);
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
static void cairo_register_pdo_pgsql(void)
{
	if (zend_hash_str_exists(&module_registry, "pdo", 3)
		&& !zend_hash_str_exists(&module_registry, "pdo_pgsql", 9)) {
		php_pdo_register_driver(&cairo_pdo_pgsql_driver);
	}
}

const cairo_extension cairo_pdo_pgsql = {.startup = cairo_register_pdo_pgsql};

package sapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/cairophp/cairo/internal/core"
	"github.com/cairophp/cairo/internal/wire"
)

// Cairo\Sql: PHP queries through Go's database/sql (pgx for PostgreSQL,
// go-sql-driver/mysql for MySQL and MariaDB).
//
// A pool belongs to the process, not to a PHP thread: Db::open() runs on
// every thread with the same DSN and options and gets the same *sql.DB, so
// max_open bounds the connections of the whole server. Queries run on the
// calling fiber's goroutine, so only the fiber waits, and the request's
// deadline cancels them.
//
// Values cross as JSON. Binary values (bytea, BLOB) travel as
// {"$bytes": base64}, both ways; times as RFC 3339 strings. PHP sends a
// DateTimeInterface parameter as {"$time": RFC 3339}: PostgreSQL gets the
// string, MySQL a time.Time, since MySQL does not parse UTC offsets.

func init() {
	registerPHP("sql.php")
	core.RegisterOp("sql.open", sqlOp(sqlOpen))
	core.RegisterOp("sql.query", sqlOp(sqlQuery))
	core.RegisterOp("sql.begin", sqlOp(sqlBegin))
	core.RegisterOp("sql.end", sqlOp(sqlEnd))
	core.RegisterOp("sql.stats", sqlOp(sqlStats))
}

type sqlRequest struct {
	// sql.open
	DSN            string  `json:"dsn"`
	MaxOpen        int     `json:"max_open"`
	MaxIdle        int     `json:"max_idle"`
	MaxLifetime    float64 `json:"max_lifetime"`    // seconds
	MaxIdleTime    float64 `json:"max_idle_time"`   // seconds
	AcquireTimeout float64 `json:"acquire_timeout"` // seconds; 0: until the request ends
	// sql.query, sql.begin, sql.stats
	Pool      int64             `json:"pool"`
	Tx        int64             `json:"tx"`
	SQL       string            `json:"sql"`
	Args      []json.RawMessage `json:"args"`
	Exec      bool              `json:"exec"`
	Isolation string            `json:"isolation"`
	ReadOnly  bool              `json:"read_only"`
	// sql.end
	Commit bool `json:"commit"`
}

// sqlOp decodes the request and reports SQL failures inside the result, as
// {"error", "sqlstate"}, so PHP can throw Cairo\Sql\SqlException.
func sqlOp(fn func(context.Context, sqlRequest) (any, error)) core.GoOp {
	return func(ctx context.Context, payload []byte) (any, error) {
		var req sqlRequest
		if err := wire.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		res, err := fn(ctx, req)
		if err != nil {
			out := map[string]any{"error": err.Error()}
			var pe *pgconn.PgError
			var me *mysql.MySQLError
			switch {
			case errors.As(err, &pe):
				out["error"] = pe.Message
				out["sqlstate"] = pe.Code
				if pe.Detail != "" {
					out["detail"] = pe.Detail
				}
			case errors.As(err, &me):
				out["error"] = me.Message
				if me.SQLState != [5]byte{} {
					out["sqlstate"] = string(me.SQLState[:])
				}
				out["detail"] = fmt.Sprintf("MySQL error %d", me.Number)
			case errors.Is(err, errSqlPoolTimeout):
				out["pool_timeout"] = true
			}
			return out, nil
		}
		return res, nil
	}
}

type sqlPoolKey struct {
	dsn                      string
	maxOpen, maxIdle         int
	maxLifetime, maxIdleTime float64
	wait                     float64
}

// sqlDB is a pool and the database behind it.
type sqlDB struct {
	*sql.DB
	mysql bool
	wait  time.Duration // acquire_timeout
}

// sqlTx is a transaction and the database behind it.
type sqlTx struct {
	*sql.Tx
	mysql bool
	conn  *sql.Conn // with acquire_timeout: closed when the transaction ends
}

var (
	sqlMu     sync.Mutex
	sqlPools  = map[sqlPoolKey]int64{}
	sqlByID   = map[int64]sqlDB{}
	sqlTxs    sync.Map // id -> sqlTx
	sqlNextID atomic.Int64
)

func sqlOpen(_ context.Context, req sqlRequest) (any, error) {
	dsn := req.DSN
	driver := "pgx"
	switch {
	case strings.HasPrefix(dsn, "mysql://"):
		var err error
		if dsn, err = mysqlDSN(dsn); err != nil {
			return nil, err
		}
		driver = "mysql"
	case strings.HasPrefix(dsn, "postgres://"), strings.HasPrefix(dsn, "postgresql://"), strings.Contains(dsn, "="):
	default:
		return nil, fmt.Errorf("Cairo\\Sql needs a postgres:// or mysql:// URL, or a PostgreSQL key=value DSN")
	}
	key := sqlPoolKey{req.DSN, req.MaxOpen, req.MaxIdle, req.MaxLifetime, req.MaxIdleTime, req.AcquireTimeout}
	sqlMu.Lock()
	defer sqlMu.Unlock()
	if id, ok := sqlPools[key]; ok {
		return id, nil
	}
	db, err := sqlConnect(driver, dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(req.MaxOpen)
	if req.MaxIdle > 0 {
		db.SetMaxIdleConns(req.MaxIdle)
	}
	if req.MaxLifetime > 0 {
		db.SetConnMaxLifetime(time.Duration(req.MaxLifetime * float64(time.Second)))
	}
	if req.MaxIdleTime > 0 {
		db.SetConnMaxIdleTime(time.Duration(req.MaxIdleTime * float64(time.Second)))
	}
	id := sqlNextID.Add(1)
	sqlPools[key] = id
	sqlByID[id] = sqlDB{db, driver == "mysql", time.Duration(req.AcquireTimeout * float64(time.Second))}
	return id, nil
}

func sqlConnect(driver, dsn string) (*sql.DB, error) {
	if driver == "mysql" {
		return sql.Open(driver, dsn)
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	return stdlib.OpenDB(*cfg, stdlib.OptionResetSession(pgResetSession)), nil
}

// mysqlDSN turns mysql://user:pass@host:port/db?param=value into the
// driver's user:pass@tcp(host:port)/db?param=value. Times are read as
// time.Time in UTC unless the URL sets parseTime or loc itself.
func mysqlDSN(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("bad mysql:// URL: %w", err)
	}
	cfg := mysql.NewConfig()
	if u.User != nil {
		cfg.User = u.User.Username()
		cfg.Passwd, _ = u.User.Password()
	}
	cfg.Net = "tcp"
	cfg.Addr = u.Host
	if u.Port() == "" {
		cfg.Addr = u.Hostname() + ":3306"
	}
	cfg.DBName = strings.TrimPrefix(u.Path, "/")
	cfg.ParseTime = true
	q := u.Query()
	if s := q.Get("socket"); s != "" {
		cfg.Net, cfg.Addr = "unix", s
		q.Del("socket")
	}
	if len(q) > 0 {
		// The driver parses its own parameters (parseTime, loc, tls, timeout, ...).
		parsed, err := mysql.ParseDSN(cfg.FormatDSN() + "?" + q.Encode())
		if err != nil {
			return "", err
		}
		cfg = parsed
	}
	if cfg.MultiStatements {
		// "SELECT 1; BEGIN" would open a transaction the pool cannot see.
		return "", errors.New("multiStatements is not supported: run one statement per query")
	}
	if !q.Has("interpolateParams") {
		// One round trip per query instead of three (prepare, execute,
		// close). The driver escapes for the connection's charset and
		// refuses the charsets where that is not safe.
		cfg.InterpolateParams = true
	}
	return cfg.FormatDSN(), nil
}

func sqlPool(id int64) (sqlDB, error) {
	sqlMu.Lock()
	defer sqlMu.Unlock()
	if db, ok := sqlByID[id]; ok {
		return db, nil
	}
	return sqlDB{}, fmt.Errorf("unknown database %d", id)
}

type sqlRunner interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

var errSqlPoolTimeout = errors.New("no connection became free")

// conn takes a connection from the pool, waiting at most acquire_timeout.
// Close gives it back.
func (db sqlDB) conn(ctx context.Context) (*sql.Conn, error) {
	wctx, cancel := context.WithTimeout(ctx, db.wait)
	defer cancel()
	c, err := db.Conn(wctx)
	if err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		return nil, fmt.Errorf("%w in %s: all %d are in use", errSqlPoolTimeout, db.wait, db.Stats().MaxOpenConnections)
	}
	return c, err
}

// sqlTarget is the pool, pool connection or transaction a statement runs
// on, whether it is MySQL, and what gives a pool connection back.
func sqlTarget(ctx context.Context, req sqlRequest) (sqlRunner, bool, func(), error) {
	if req.Tx != 0 {
		if tx, ok := sqlTxs.Load(req.Tx); ok {
			return tx.(sqlTx).Tx, tx.(sqlTx).mysql, func() {}, nil
		}
		return nil, false, nil, errors.New("the transaction has ended")
	}
	if sqlLeavesState(req.SQL) {
		return nil, false, nil, errSessionStatement
	}
	db, err := sqlPool(req.Pool)
	if err != nil || db.wait == 0 {
		return db.DB, db.mysql, func() {}, err
	}
	c, err := db.conn(ctx)
	if err != nil {
		return nil, false, nil, err
	}
	return c, db.mysql, func() { _ = c.Close() }, nil
}

func sqlQuery(ctx context.Context, req sqlRequest) (any, error) {
	target, isMySQL, release, err := sqlTarget(ctx, req)
	if err != nil {
		return nil, err
	}
	defer release()
	args := make([]any, len(req.Args))
	for i, raw := range req.Args {
		if args[i], err = sqlArg(raw, isMySQL); err != nil {
			return nil, fmt.Errorf("parameter %d: %w", i+1, err)
		}
	}
	if req.Exec {
		res, err := target.ExecContext(ctx, req.SQL, args...)
		if err != nil {
			return nil, err
		}
		n, _ := res.RowsAffected()
		return map[string]any{"affected": n}, nil
	}
	rows, err := target.QueryContext(ctx, req.SQL, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	// MySQL sends text columns as bytes and DATE as a time: tell them apart
	// from binary columns by the column's type.
	var kinds []string
	if isMySQL {
		types, err := rows.ColumnTypes()
		if err != nil {
			return nil, err
		}
		kinds = make([]string, len(types))
		for i, ct := range types {
			kinds[i] = ct.DatabaseTypeName()
		}
	}
	out := [][]any{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		for i, v := range vals {
			if kinds != nil {
				v = mysqlValue(kinds[i], v)
			}
			vals[i] = sqlValue(v)
		}
		out = append(out, vals)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return map[string]any{"columns": cols, "rows": out}, nil
}

// mysqlValue turns MySQL's bytes into a string unless the column is binary,
// and a DATE into Y-m-d.
func mysqlValue(kind string, v any) any {
	switch x := v.(type) {
	case []byte:
		switch kind {
		case "BINARY", "VARBINARY", "TINYBLOB", "BLOB", "MEDIUMBLOB", "LONGBLOB", "BIT", "GEOMETRY":
			return x
		}
		return string(x)
	case time.Time:
		if kind == "DATE" {
			return x.Format(time.DateOnly)
		}
	}
	return v
}

// sqlArg turns a JSON parameter into a driver value: integers stay exact.
func sqlArg(raw json.RawMessage, isMySQL bool) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i, nil
		}
		return x.Float64()
	case map[string]any:
		if b, ok := x["$bytes"].(string); ok && len(x) == 1 {
			return base64.StdEncoding.DecodeString(b)
		}
		if t, ok := x["$time"].(string); ok && len(x) == 1 {
			if !isMySQL {
				return t, nil
			}
			return time.Parse(time.RFC3339Nano, t)
		}
		return nil, errors.New("arrays and objects are not SQL values: json_encode() them")
	case []any:
		return nil, errors.New("arrays and objects are not SQL values: json_encode() them")
	}
	return v, nil
}

// sqlValue makes a scanned value safe for JSON.
func sqlValue(v any) any {
	switch x := v.(type) {
	case []byte:
		return map[string]string{"$bytes": base64.StdEncoding.EncodeToString(x)}
	case time.Time:
		// In UTC, so the result does not depend on the server's time zone.
		return x.UTC().Format(time.RFC3339Nano)
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return fmt.Sprint(x)
		}
	case float32:
		return sqlValue(float64(x))
	case fmt.Stringer:
		return x.String()
	}
	return v
}

func sqlBegin(ctx context.Context, req sqlRequest) (any, error) {
	db, err := sqlPool(req.Pool)
	if err != nil {
		return nil, err
	}
	opts := &sql.TxOptions{ReadOnly: req.ReadOnly}
	switch strings.ToLower(req.Isolation) {
	case "":
	case "read committed":
		opts.Isolation = sql.LevelReadCommitted
	case "repeatable read":
		opts.Isolation = sql.LevelRepeatableRead
	case "serializable":
		opts.Isolation = sql.LevelSerializable
	default:
		return nil, fmt.Errorf("unknown isolation level %q", req.Isolation)
	}
	// Bound to the fiber that began it: if that fiber ends (or its request's
	// deadline passes) without commit or rollback, database/sql rolls back.
	t := sqlTx{mysql: db.mysql}
	if db.wait > 0 {
		if t.conn, err = db.conn(ctx); err != nil {
			return nil, err
		}
		t.Tx, err = t.conn.BeginTx(ctx, opts)
	} else {
		t.Tx, err = db.BeginTx(ctx, opts)
	}
	if err != nil {
		t.close()
		return nil, err
	}
	id := sqlNextID.Add(1)
	sqlTxs.Store(id, t)
	go func() {
		<-ctx.Done()
		if v, ok := sqlTxs.LoadAndDelete(id); ok {
			v.(sqlTx).close() // waits for database/sql's rollback
		}
	}()
	return id, nil
}

// close gives back the pool connection of a transaction begun with
// acquire_timeout, once the transaction has ended.
func (t sqlTx) close() {
	if t.conn != nil {
		_ = t.conn.Close()
	}
}

func sqlEnd(_ context.Context, req sqlRequest) (any, error) {
	v, ok := sqlTxs.LoadAndDelete(req.Tx)
	if !ok {
		return nil, errors.New("the transaction has ended")
	}
	t := v.(sqlTx)
	defer t.close()
	if req.Commit {
		return true, t.Commit()
	}
	return true, t.Rollback()
}

func sqlStats(_ context.Context, req sqlRequest) (any, error) {
	db, err := sqlPool(req.Pool)
	if err != nil {
		return nil, err
	}
	s := db.Stats()
	return map[string]any{
		"max_open":   s.MaxOpenConnections,
		"open":       s.OpenConnections,
		"in_use":     s.InUse,
		"idle":       s.Idle,
		"wait_count": s.WaitCount,
		"wait_time":  s.WaitDuration.Seconds(),
	}, nil
}

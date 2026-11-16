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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
)

// Cairo\Sql: PHP queries through Go's database/sql (pgx for PostgreSQL).
//
// A pool belongs to the process, not to a PHP thread: Db::open() runs on
// every thread with the same DSN and options and gets the same *sql.DB, so
// max_open bounds the connections of the whole server. Queries run on the
// calling fiber's goroutine, so only the fiber waits, and the request's
// deadline cancels them.
//
// Values cross as JSON. Binary values (bytea) travel as {"$bytes": base64},
// both ways; times as RFC 3339 strings.

func init() {
	registerOp("sql.open", sqlOp(sqlOpen))
	registerOp("sql.query", sqlOp(sqlQuery))
	registerOp("sql.begin", sqlOp(sqlBegin))
	registerOp("sql.end", sqlOp(sqlEnd))
	registerOp("sql.stats", sqlOp(sqlStats))
}

type sqlRequest struct {
	// sql.open
	DSN         string  `json:"dsn"`
	MaxOpen     int     `json:"max_open"`
	MaxIdle     int     `json:"max_idle"`
	MaxLifetime float64 `json:"max_lifetime"`  // seconds
	MaxIdleTime float64 `json:"max_idle_time"` // seconds
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
func sqlOp(fn func(context.Context, sqlRequest) (any, error)) goOp {
	return func(ctx context.Context, payload []byte) (any, error) {
		var req sqlRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		res, err := fn(ctx, req)
		if err != nil {
			out := map[string]any{"error": err.Error()}
			var pe *pgconn.PgError
			if errors.As(err, &pe) {
				out["error"] = pe.Message
				out["sqlstate"] = pe.Code
				if pe.Detail != "" {
					out["detail"] = pe.Detail
				}
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
}

var (
	sqlMu     sync.Mutex
	sqlPools  = map[sqlPoolKey]int64{}
	sqlByID   = map[int64]*sql.DB{}
	sqlTxs    sync.Map // id -> *sql.Tx
	sqlNextID atomic.Int64
)

func sqlOpen(_ context.Context, req sqlRequest) (any, error) {
	dsn := req.DSN
	switch {
	case strings.HasPrefix(dsn, "postgres://"), strings.HasPrefix(dsn, "postgresql://"), strings.Contains(dsn, "="):
	default:
		return nil, fmt.Errorf("Cairo\\Sql supports PostgreSQL only for now: use a postgres:// URL or a key=value DSN")
	}
	key := sqlPoolKey{dsn, req.MaxOpen, req.MaxIdle, req.MaxLifetime, req.MaxIdleTime}
	sqlMu.Lock()
	defer sqlMu.Unlock()
	if id, ok := sqlPools[key]; ok {
		return id, nil
	}
	db, err := sql.Open("pgx", dsn)
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
	sqlByID[id] = db
	return id, nil
}

func sqlPool(id int64) (*sql.DB, error) {
	sqlMu.Lock()
	defer sqlMu.Unlock()
	if db, ok := sqlByID[id]; ok {
		return db, nil
	}
	return nil, fmt.Errorf("unknown database %d", id)
}

type sqlRunner interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func sqlTarget(req sqlRequest) (sqlRunner, error) {
	if req.Tx != 0 {
		if tx, ok := sqlTxs.Load(req.Tx); ok {
			return tx.(*sql.Tx), nil
		}
		return nil, errors.New("the transaction has ended")
	}
	return sqlPool(req.Pool)
}

func sqlQuery(ctx context.Context, req sqlRequest) (any, error) {
	target, err := sqlTarget(req)
	if err != nil {
		return nil, err
	}
	args := make([]any, len(req.Args))
	for i, raw := range req.Args {
		if args[i], err = sqlArg(raw); err != nil {
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
			vals[i] = sqlValue(v)
		}
		out = append(out, vals)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return map[string]any{"columns": cols, "rows": out}, nil
}

// sqlArg turns a JSON parameter into a driver value: integers stay exact.
func sqlArg(raw json.RawMessage) (any, error) {
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
		return x.Format(time.RFC3339Nano)
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
	tx, err := db.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	id := sqlNextID.Add(1)
	sqlTxs.Store(id, tx)
	go func() {
		<-ctx.Done()
		sqlTxs.Delete(id)
	}()
	return id, nil
}

func sqlEnd(_ context.Context, req sqlRequest) (any, error) {
	v, ok := sqlTxs.LoadAndDelete(req.Tx)
	if !ok {
		return nil, errors.New("the transaction has ended")
	}
	if req.Commit {
		return true, v.(*sql.Tx).Commit()
	}
	return true, v.(*sql.Tx).Rollback()
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

package sapi

/*
#include <stdlib.h>
#include "pgsql.h"
*/
import "C"

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/jackc/pgx/v5/pgconn"
)

// Cairo's PDO driver for PostgreSQL ("pgsql:" DSNs) is C on the PHP side and
// pgx's pgconn on the Go side. PHP asks for one operation at a time; inside a
// request fiber it runs on the fiber's goroutine (the fiber suspends, and the
// request's deadline cancels the query), elsewhere the PHP thread waits for
// it. Results are read in full, like libpq's PQexec, and handed to C.

type pgConn struct {
	mu     sync.Mutex
	conn   *pgconn.PgConn // nil while a shared-pool handle holds none
	notify *pgNotifyQueue

	// A handle on a pool shared by all threads (pgshared.go): the request
	// its connection is bound to, and how to stop the give-back.
	shared    *pgShared
	boundTo   context.Context
	unbind    func()
	gen       int64
	releasing atomic.Bool // the give-back holds mu, not another fiber
}

// pgNotifyQueue holds LISTEN/NOTIFY messages that arrived during any
// operation until PDO::pgsqlGetNotify() takes them.
type pgNotifyQueue struct {
	mu   sync.Mutex
	list []*pgconn.Notification
}

func (q *pgNotifyQueue) push(n *pgconn.Notification) {
	q.mu.Lock()
	q.list = append(q.list, n)
	q.mu.Unlock()
}

func (q *pgNotifyQueue) pop() *pgconn.Notification {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.list) == 0 {
		return nil
	}
	n := q.list[0]
	q.list = q.list[1:]
	return n
}

var (
	pgConns   sync.Map // int64 -> *pgConn
	pgResults sync.Map // int64 -> *C.cairo_pg_result
	pgNextID  atomic.Int64
)

type pgRequest struct {
	Kind     string    `json:"kind"` // connect, exec, query, close, status, notify, copy_in, copy_out, lo
	DSN      string    `json:"dsn"`
	User     string    `json:"user"`
	Password string    `json:"password"`
	Timeout  float64   `json:"timeout"`
	Conn     int64     `json:"conn"`
	SQL      string    `json:"sql"`
	Params   []*string `json:"params"`  // base64, nil for SQL NULL
	Binary   []int     `json:"binary"`  // indexes of LOB parameters: raw bytea
	Data     string    `json:"data"`    // copy_in: base64 COPY text
	WaitMS   int64     `json:"wait_ms"` // notify: how long to wait for one

	// lo: large objects, through the server's lo_* functions.
	LO     string `json:"lo"` // create, open, read, write, seek, tell, close, unlink
	OID    int64  `json:"oid"`
	FD     int64  `json:"fd"`
	Mode   int64  `json:"mode"`   // open: INV_READ | INV_WRITE
	N      int64  `json:"n"`      // read: up to n bytes
	Offset int64  `json:"offset"` // seek
	Whence int64  `json:"whence"` // seek: 0 set, 1 current, 2 end
}

// pgDo runs one request. The response always has "ok"; failures carry
// "sqlstate" and "error" (libpq style: "ERROR:  ...").
func pgDo(ctx context.Context, req pgRequest) map[string]any {
	switch req.Kind {
	case "connect":
		return pgConnect(ctx, req)
	case "close":
		if v, ok := pgConns.LoadAndDelete(req.Conn); ok {
			pc := v.(*pgConn)
			pc.mu.Lock()
			if pc.shared != nil {
				if pc.unbind != nil {
					pc.unbind()
				}
				pgDetach(pc)
			} else {
				cctx, cancel := context.WithTimeout(context.Background(), time.Second)
				_ = pc.conn.Close(cctx)
				cancel()
			}
			pc.mu.Unlock()
		}
		return map[string]any{"ok": true}
	}
	v, ok := pgConns.Load(req.Conn)
	if !ok {
		return pgFail("08003", "connection is closed")
	}
	pc := v.(*pgConn)
	if !pc.mu.TryLock() {
		if !pc.releasing.Load() {
			return pgFail("HY000", "the connection is busy with another query (use one PDO per fiber, e.g. through Cairo\\Pool)")
		}
		// The last request's give-back may still hold it for a moment.
		pc.mu.Lock()
	}
	defer pc.mu.Unlock()
	if pc.shared != nil {
		if req.Kind == "notify" {
			return pgFail("HY000", "LISTEN needs a connection of its own: drop cairo_pool from the DSN")
		}
		if err := pgAttach(ctx, pc); err != nil {
			return pgError(err)
		}
		defer pgAfterOp(ctx, pc)
	}
	if pc.conn.IsClosed() {
		return pgFail("08006", "server closed the connection")
	}
	resp := pgDoOn(ctx, pc, req)
	// The driver keeps this for PDO::inTransaction().
	resp["tx"] = string(rune(pc.conn.TxStatus()))
	return resp
}

// pgDoOn runs a request on its connection, which the caller has locked.
func pgDoOn(ctx context.Context, pc *pgConn, req pgRequest) map[string]any {
	switch req.Kind {
	case "status":
		return map[string]any{"ok": true, "server_version": pc.conn.ParameterStatus("server_version"), "pid": pc.conn.PID()}
	case "notify":
		return pgNotify(ctx, pc, req.WaitMS)
	case "copy_in":
		data, err := base64.StdEncoding.DecodeString(req.Data)
		if err != nil {
			return pgFail("HY000", "bad COPY data encoding")
		}
		tag, err := pc.conn.CopyFrom(ctx, bytes.NewReader(data), req.SQL)
		if err != nil {
			return pgError(err)
		}
		return map[string]any{"ok": true, "affected": tag.RowsAffected()}
	case "lo":
		return pgLargeObject(ctx, pc, req)
	case "copy_out":
		var buf bytes.Buffer
		if _, err := pc.conn.CopyTo(ctx, &buf, req.SQL); err != nil {
			return pgError(err)
		}
		return map[string]any{"ok": true, "data": base64.StdEncoding.EncodeToString(buf.Bytes())}
	case "exec":
		// PDO::exec(): the simple protocol, so several statements may be sent.
		results, err := pc.conn.Exec(ctx, req.SQL).ReadAll()
		if err != nil {
			return pgError(err)
		}
		var affected int64
		if len(results) > 0 {
			affected = results[len(results)-1].CommandTag.RowsAffected()
		}
		return map[string]any{"ok": true, "affected": affected}
	case "query":
		values := make([][]byte, len(req.Params))
		for i, p := range req.Params {
			if p == nil {
				continue
			}
			b, err := base64.StdEncoding.DecodeString(*p)
			if err != nil {
				return pgFail("HY000", "bad parameter encoding")
			}
			if b == nil {
				b = []byte{}
			}
			values[i] = b
		}
		// Parameters go as text with unspecified types, as pdo_pgsql sends
		// them: the server infers each type from the query. LOBs go as
		// binary bytea.
		var oids []uint32
		var formats []int16
		if len(req.Binary) > 0 {
			oids = make([]uint32, len(values))
			formats = make([]int16, len(values))
			for _, i := range req.Binary {
				if i >= 0 && i < len(values) {
					oids[i], formats[i] = 17, 1
				}
			}
		}
		rr := pc.conn.ExecParams(ctx, req.SQL, values, oids, formats, nil)
		res := pgCollect(rr)
		if res.err != nil {
			res.free()
			return pgError(res.err)
		}
		id := pgNextID.Add(1)
		pgResults.Store(id, res.c)
		return map[string]any{"ok": true, "result": id}
	}
	return pgFail("HY000", "unknown pgsql request "+req.Kind)
}

func pgConnect(ctx context.Context, req pgRequest) map[string]any {
	dsn, shared, err := pgSharedOption(req.DSN)
	if err != nil {
		return pgFail("08006", err.Error())
	}
	cfg, err := pgconn.ParseConfig(pgDSN(dsn))
	if err != nil {
		return pgFail("08006", err.Error())
	}
	if req.User != "" {
		cfg.User = req.User
	}
	if req.Password != "" {
		cfg.Password = req.Password
	}
	if req.Timeout > 0 {
		cfg.ConnectTimeout = time.Duration(req.Timeout * float64(time.Second))
	}
	if shared > 0 {
		return pgConnectShared(ctx, cfg, dsn+"\x00"+req.User+"\x00"+req.Password, shared)
	}
	queue := &pgNotifyQueue{}
	cfg.OnNotification = func(_ *pgconn.PgConn, n *pgconn.Notification) { queue.push(n) }
	conn, err := pgconn.ConnectConfig(ctx, cfg)
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) {
			return pgError(err)
		}
		return pgFail("08006", err.Error())
	}
	id := pgNextID.Add(1)
	pgConns.Store(id, &pgConn{conn: conn, notify: queue})
	return map[string]any{"ok": true, "conn": id}
}

// pgNotify returns the oldest queued notification, waiting up to waitMS
// milliseconds for one to arrive ("notification": null when none did). A
// wait that runs out leaves the connection usable.
func pgNotify(ctx context.Context, pc *pgConn, waitMS int64) map[string]any {
	n := pc.notify.pop()
	if n == nil {
		if waitMS < 1 {
			waitMS = 1 // still read what the server already sent
		}
		wctx, cancel := context.WithTimeout(ctx, time.Duration(waitMS)*time.Millisecond)
		err := pc.conn.WaitForNotification(wctx)
		cancel()
		if err != nil && (ctx.Err() != nil || !errors.Is(err, context.DeadlineExceeded)) {
			return pgError(err)
		}
		n = pc.notify.pop()
	}
	if n == nil {
		return map[string]any{"ok": true, "notification": nil}
	}
	return map[string]any{"ok": true, "notification": map[string]any{"channel": n.Channel, "pid": n.PID, "payload": n.Payload}}
}

// pgLargeObject runs one large-object operation as a single statement on
// the server's lo_* functions. Descriptors from lo_open last until the end
// of the transaction, so PDO needs one open, as with pdo_pgsql. The answer
// carries "value" (an integer) or, for read, "data" (base64).
func pgLargeObject(ctx context.Context, pc *pgConn, req pgRequest) map[string]any {
	num := func(n int64) []byte { return []byte(strconv.FormatInt(n, 10)) }
	var (
		sql     string
		params  [][]byte
		formats []int16
	)
	switch req.LO {
	case "create":
		sql = "SELECT lo_creat(-1)"
	case "open":
		sql, params = "SELECT lo_open($1::oid, $2::int)", [][]byte{num(req.OID), num(req.Mode)}
	case "read":
		sql, params = "SELECT loread($1::int, $2::int)", [][]byte{num(req.FD), num(req.N)}
	case "write":
		data, err := base64.StdEncoding.DecodeString(req.Data)
		if err != nil {
			return pgFail("HY000", "bad large object data encoding")
		}
		if data == nil {
			data = []byte{}
		}
		sql, params, formats = "SELECT lowrite($1::int, $2::bytea)", [][]byte{num(req.FD), data}, []int16{0, 1}
	case "seek":
		sql, params = "SELECT lo_lseek64($1::int, $2::bigint, $3::int)", [][]byte{num(req.FD), num(req.Offset), num(req.Whence)}
	case "tell":
		sql, params = "SELECT lo_tell64($1::int)", [][]byte{num(req.FD)}
	case "close":
		sql, params = "SELECT lo_close($1::int)", [][]byte{num(req.FD)}
	case "unlink":
		sql, params = "SELECT lo_unlink($1::oid)", [][]byte{num(req.OID)}
	default:
		return pgFail("HY000", "unknown large object operation "+req.LO)
	}
	var oids []uint32
	if formats != nil {
		oids = make([]uint32, len(params))
		for i, f := range formats {
			if f == 1 {
				oids[i] = 17 // bytea
			}
		}
	}
	// read gets its bytes in binary; the rest are integers in text.
	resultFormats := []int16{0}
	if req.LO == "read" {
		resultFormats[0] = 1
	}
	res := pc.conn.ExecParams(ctx, sql, params, oids, formats, resultFormats).Read()
	if res.Err != nil {
		return pgError(res.Err)
	}
	if len(res.Rows) != 1 || len(res.Rows[0]) != 1 || res.Rows[0][0] == nil {
		return pgFail("HY000", "unexpected large object result")
	}
	v := res.Rows[0][0]
	if req.LO == "read" {
		return map[string]any{"ok": true, "data": base64.StdEncoding.EncodeToString(v)}
	}
	n, err := strconv.ParseInt(string(v), 10, 64)
	if err != nil {
		return pgFail("HY000", "unexpected large object result "+string(v))
	}
	return map[string]any{"ok": true, "value": n}
}

func pgFail(state, msg string) map[string]any {
	return map[string]any{"ok": false, "sqlstate": state, "error": msg}
}

func pgError(err error) map[string]any {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		msg := pe.Severity + ":  " + pe.Message
		if pe.Detail != "" {
			msg += "\nDETAIL:  " + pe.Detail
		}
		return pgFail(pe.Code, msg)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return pgFail("57014", "canceling statement: "+err.Error())
	}
	return pgFail("08006", err.Error())
}

type pgResult struct {
	c   *C.cairo_pg_result
	err error
}

func (r pgResult) free() {
	if r.c != nil {
		C.cairo_pg_result_free(r.c)
	}
}

// pgCollect reads a whole result into C memory.
func pgCollect(rr *pgconn.ResultReader) pgResult {
	fields := rr.FieldDescriptions()
	var rows [][][]byte
	for rr.NextRow() {
		src := rr.Values()
		row := make([][]byte, len(src))
		for i, v := range src {
			if v != nil {
				row[i] = append([]byte{}, v...)
			}
		}
		rows = append(rows, row)
	}
	tag, err := rr.Close()
	if err != nil {
		return pgResult{err: err}
	}
	r := (*C.cairo_pg_result)(C.calloc(1, C.size_t(unsafe.Sizeof(C.cairo_pg_result{}))))
	r.ncols = C.int(len(fields))
	r.nrows = C.int(len(rows))
	r.affected = C.longlong(tag.RowsAffected())
	if n := len(fields); n > 0 {
		r.names = (**C.char)(C.calloc(C.size_t(n), C.size_t(unsafe.Sizeof(uintptr(0)))))
		r.oids = (*C.uint)(C.calloc(C.size_t(n), C.size_t(unsafe.Sizeof(C.uint(0)))))
		names := unsafe.Slice(r.names, n)
		oids := unsafe.Slice(r.oids, n)
		for i, f := range fields {
			names[i] = C.CString(f.Name)
			oids[i] = C.uint(f.DataTypeOID)
		}
	}
	if cells := len(rows) * len(fields); cells > 0 {
		r.vals = (**C.char)(C.calloc(C.size_t(cells), C.size_t(unsafe.Sizeof(uintptr(0)))))
		r.lens = (*C.int)(C.calloc(C.size_t(cells), C.size_t(unsafe.Sizeof(C.int(0)))))
		vals := unsafe.Slice(r.vals, cells)
		lens := unsafe.Slice(r.lens, cells)
		for i, row := range rows {
			for j, v := range row {
				if v == nil {
					continue
				}
				k := i*len(fields) + j
				p := (*C.char)(C.malloc(C.size_t(len(v) + 1)))
				buf := unsafe.Slice((*byte)(unsafe.Pointer(p)), len(v)+1)
				copy(buf, v)
				buf[len(v)] = 0
				vals[k] = p
				lens[k] = C.int(len(v))
			}
		}
	}
	return pgResult{c: r}
}

func pgRequestFrom(data []byte) (pgRequest, error) {
	var req pgRequest
	err := json.Unmarshal(data, &req)
	return req, err
}

// The "pg" fiber operation: one call of the PDO driver on the fiber's
// goroutine, so the request's deadline cancels the query.
func init() {
	registerFiberOp("pg", func(ctx context.Context, payload []byte) (any, string) {
		req, err := pgRequestFrom(payload)
		if err != nil {
			return nil, err.Error()
		}
		resp := pgDo(ctx, req)
		if ctx.Err() != nil {
			if id, ok := resp["result"].(int64); ok {
				if r := goPgTake(C.longlong(id)); r != nil {
					C.cairo_pg_result_free(r)
				}
			}
			return nil, ctxError(ctx)
		}
		return resp, ""
	})
}

// goPgSync runs a request on the calling PHP thread (outside a Cairo fiber,
// or with hooks off). Returns the JSON response; the caller frees it.
//
//export goPgSync
func goPgSync(data *C.char, n C.int) *C.char {
	req, err := pgRequestFrom(C.GoBytes(unsafe.Pointer(data), n))
	var resp map[string]any
	if err != nil {
		resp = pgFail("HY000", err.Error())
	} else {
		resp = pgDo(context.Background(), req)
	}
	b, _ := json.Marshal(resp)
	return C.CString(string(b))
}

// goPgTake hands C the result set stored under id (once).
//
//export goPgTake
func goPgTake(id C.longlong) *C.cairo_pg_result {
	if v, ok := pgResults.LoadAndDelete(int64(id)); ok {
		return v.(*C.cairo_pg_result)
	}
	return nil
}

// pgDSN turns a PDO DSN body ("host=x;port=5432;dbname=y") into the
// keyword/value form pgconn parses; a postgres:// URL is passed through.
func pgDSN(s string) string {
	if strings.HasPrefix(s, "postgres://") || strings.HasPrefix(s, "postgresql://") {
		return s
	}
	return strings.ReplaceAll(s, ";", " ")
}

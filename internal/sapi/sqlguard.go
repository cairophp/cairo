package sapi

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
)

// A statement run on a Cairo\Sql pool, outside Db::transaction(), gets
// whichever connection is free and gives it back when it ends. One that
// opens a transaction, turns autocommit off or locks tables would leave that
// on the connection, and the next request to get it, on any thread, would
// run inside it. So those statements are refused on the pool, and a
// PostgreSQL connection that still comes back in a transaction (a
// multi-statement string can open one) is closed instead of reused.

var errSessionStatement = errors.New("this statement would leave a transaction or a lock on a shared pool connection; use Db::transaction()")

// sqlLeavesState reports whether q opens a transaction, turns autocommit off
// or takes table locks.
func sqlLeavesState(q string) bool {
	w, rest := sqlWord(q)
	switch {
	case strings.EqualFold(w, "BEGIN"):
		next, _ := sqlWord(rest)
		return !strings.EqualFold(next, "NOT") // MariaDB's BEGIN NOT ATOMIC is a block
	case strings.EqualFold(w, "START"):
		next, _ := sqlWord(rest)
		return strings.EqualFold(next, "TRANSACTION")
	case strings.EqualFold(w, "LOCK"), strings.EqualFold(w, "XA"):
		return true
	case strings.EqualFold(w, "SET"):
		return sqlSetsAutocommit(rest)
	}
	return false
}

// sqlSetsAutocommit matches MySQL's SET [SESSION|LOCAL] autocommit and
// SET @@[session.]autocommit.
func sqlSetsAutocommit(rest string) bool {
	w, after := sqlWord(rest)
	if strings.EqualFold(w, "SESSION") || strings.EqualFold(w, "LOCAL") {
		w, _ = sqlWord(after)
	} else if w == "" {
		s := strings.TrimLeft(rest, " \t\r\n")
		s = strings.TrimPrefix(s, "@@")
		for _, scope := range []string{"session.", "local."} {
			if len(s) >= len(scope) && strings.EqualFold(s[:len(scope)], scope) {
				s = s[len(scope):]
			}
		}
		w, _ = sqlWord(s)
	}
	return strings.EqualFold(w, "AUTOCOMMIT")
}

// sqlWord returns the first word of q after spaces and comments, and what
// follows it.
func sqlWord(q string) (word, rest string) {
	for {
		q = strings.TrimLeft(q, " \t\r\n(")
		switch {
		case strings.HasPrefix(q, "--"), strings.HasPrefix(q, "#"):
			_, q, _ = strings.Cut(q, "\n")
		case strings.HasPrefix(q, "/*!"):
			// MySQL runs the inside of /*!version ... */ as SQL.
			q = strings.TrimLeft(q[3:], "0123456789")
		case strings.HasPrefix(q, "/*"):
			if _, after, ok := strings.Cut(q[2:], "*/"); ok {
				q = after
			} else {
				q = ""
			}
		default:
			i := 0
			for i < len(q) && (q[i]|0x20 >= 'a' && q[i]|0x20 <= 'z' || q[i] == '_') {
				i++
			}
			return q[:i], q[i:]
		}
	}
}

// pgResetSession runs when database/sql hands out a PostgreSQL connection
// again: one still in a transaction is closed, which rolls it back.
func pgResetSession(_ context.Context, c *pgx.Conn) error {
	if c.PgConn().TxStatus() != 'I' {
		return driver.ErrBadConn
	}
	return nil
}

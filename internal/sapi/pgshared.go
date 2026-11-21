package sapi

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// A PDO PostgreSQL connection made with "cairo_pool=N" in its DSN owns no
// server connection. It borrows one from a pool shared by every PHP thread,
// on its first statement in a request, and gives it back when that request
// ends (the fiber's context is done). So a pool of N server connections
// serves any number of PDO objects on any thread, and a thread that is busy
// gets connections another thread is not using.
//
// A transaction keeps its connection: a handle in a transaction is never
// given back, and one left open when the request ends is rolled back first.

const maxSharedPool = 10000

type pgShared struct {
	cfg   *pgconn.Config
	max   int
	slots chan struct{}       // one per open server connection
	idle  chan *pgconn.PgConn // open and free
}

var (
	pgSharedMu    sync.Mutex
	pgSharedPools = map[string]*pgShared{}
)

// pgSharedOption takes "cairo_pool=N" out of a PDO DSN body; n is 0 when it
// is not there.
func pgSharedOption(dsn string) (rest string, n int, err error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return dsn, 0, err
		}
		q := u.Query()
		v := q.Get("cairo_pool")
		if v == "" {
			return dsn, 0, nil
		}
		q.Del("cairo_pool")
		u.RawQuery = q.Encode()
		n, err = pgSharedSize(v)
		return u.String(), n, err
	}
	parts := strings.Split(dsn, ";")
	kept := parts[:0]
	for _, p := range parts {
		k, v, _ := strings.Cut(strings.TrimSpace(p), "=")
		if strings.TrimSpace(k) != "cairo_pool" {
			kept = append(kept, p)
			continue
		}
		if n, err = pgSharedSize(v); err != nil {
			return dsn, 0, err
		}
	}
	return strings.Join(kept, ";"), n, nil
}

func pgSharedSize(v string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 1 || n > maxSharedPool {
		return 0, errors.New("cairo_pool must be a number of connections from 1 to 10000")
	}
	return n, nil
}

// pgSharedPool returns the pool of this server and user, made on first use.
// Every thread runs server.php, so every thread asks for the same pools.
func pgSharedPool(cfg *pgconn.Config, key string, max int) (*pgShared, error) {
	pgSharedMu.Lock()
	defer pgSharedMu.Unlock()
	if p, ok := pgSharedPools[key]; ok {
		if p.max != max {
			return nil, errors.New("this database already has a shared pool of " + strconv.Itoa(p.max) + " connections")
		}
		return p, nil
	}
	cfg.OnNotification = nil // LISTEN needs a connection of its own
	p := &pgShared{cfg: cfg, max: max, slots: make(chan struct{}, max), idle: make(chan *pgconn.PgConn, max)}
	pgSharedPools[key] = p
	return p, nil
}

// pgConnectShared makes a handle on the shared pool. It borrows a connection
// once, so a wrong password or host fails here as with any PDO.
func pgConnectShared(ctx context.Context, cfg *pgconn.Config, key string, max int) map[string]any {
	p, err := pgSharedPool(cfg, key, max)
	if err != nil {
		return pgFail("08006", err.Error())
	}
	c, err := p.acquire(ctx)
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) {
			return pgError(err)
		}
		return pgFail("08006", err.Error())
	}
	p.release(c)
	id := pgNextID.Add(1)
	pgConns.Store(id, &pgConn{shared: p, notify: &pgNotifyQueue{}})
	return map[string]any{"ok": true, "conn": id}
}

// acquire returns a free connection, opens one while the pool is under its
// size, or waits for one until ctx ends.
func (p *pgShared) acquire(ctx context.Context) (*pgconn.PgConn, error) {
	for {
		var c *pgconn.PgConn
		select {
		case c = <-p.idle:
		default:
			select {
			case c = <-p.idle:
			case p.slots <- struct{}{}:
				c, err := pgconn.ConnectConfig(ctx, p.cfg)
				if err != nil {
					<-p.slots
					return nil, err
				}
				return c, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if !c.IsClosed() {
			return c, nil
		}
		<-p.slots // closed while idle: open another
	}
}

// release takes back a connection; one that is broken, or not idle (a
// transaction or a reply it could not finish), is closed instead.
func (p *pgShared) release(c *pgconn.PgConn) {
	if c.IsClosed() || c.IsBusy() || c.TxStatus() != 'I' {
		cctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = c.Close(cctx)
		cancel()
		<-p.slots
		return
	}
	p.idle <- c
}

// pgAttach gives a shared-pool handle a server connection for the request
// of ctx. The caller holds pc.mu.
func pgAttach(ctx context.Context, pc *pgConn) error {
	if pc.conn == nil {
		c, err := pc.shared.acquire(ctx)
		if err != nil {
			return err
		}
		pc.conn = c
	}
	if ctx.Done() == nil || ctx == pc.boundTo {
		return nil
	}
	// Bind it to this request: given back when the request ends. A
	// handle the next request uses before the last one's context is done
	// moves to the new request.
	if pc.unbind != nil {
		pc.unbind()
	}
	pc.boundTo = ctx
	pc.gen++
	gen := pc.gen
	stop := context.AfterFunc(ctx, func() { pgDetachIf(pc, gen) })
	pc.unbind = func() { stop() }
	return nil
}

// pgDetachIf gives the handle's connection back when the request it was
// bound to (generation gen) ended and no later request took it over.
func pgDetachIf(pc *pgConn, gen int64) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if pc.gen != gen {
		return
	}
	pgDetach(pc)
}

// pgDetach gives the handle's connection back, rolling back a transaction
// left open. The caller holds pc.mu.
func pgDetach(pc *pgConn) {
	pc.boundTo, pc.unbind = nil, nil
	pc.gen++
	c := pc.conn
	if c == nil {
		return
	}
	pc.conn = nil
	if !c.IsClosed() && c.TxStatus() != 'I' {
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = c.Exec(cctx, "ROLLBACK").ReadAll()
		cancel()
	}
	pc.shared.release(c)
}

// pgAfterOp gives the connection back right after a statement made outside
// a request (no context to wait for), unless a transaction is open.
func pgAfterOp(ctx context.Context, pc *pgConn) {
	if ctx.Done() == nil && pc.conn != nil && pc.conn.TxStatus() == 'I' {
		pgDetach(pc)
	}
}

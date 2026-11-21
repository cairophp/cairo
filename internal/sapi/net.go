package sapi

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cairophp/cairo/internal/core"
	"github.com/cairophp/cairo/internal/wire"
)

// Cairo\Net: connections, listeners and DNS from Go's net package. PHP holds
// ids; the sockets live here. Each call is a Go operation (core.RegisterOp), so in a
// request fiber only the fiber waits, and the request's deadline interrupts
// a blocked read, write or accept.

type netConn struct {
	conn  net.Conn
	rmu   sync.Mutex // one reader at a time (the buffered reader)
	r     *bufio.Reader
	wmu   sync.Mutex
	close sync.Once
}

type netListener struct {
	ln  net.Listener
	raw deadliner // the TCP or unix listener, for accept deadlines
}

var (
	netConns     sync.Map // int64 -> *netConn
	netListeners sync.Map // int64 -> *netListener
	netPackets   sync.Map // int64 -> net.PacketConn
	netNextID    atomic.Int64
)

type netRequest struct {
	ID      int64       `json:"id"`
	Network string      `json:"network"`
	Address string      `json:"address"`
	Timeout float64     `json:"timeout"` // seconds, 0 for none
	TLS     *tlsOptions `json:"tls"`
	Cert    string      `json:"cert"` // listen("tls"): server certificate (and key)
	Key     string      `json:"key"`
	Max     int         `json:"max"`
	Mode    string      `json:"mode"` // read: "some", "exact", "line"
	EOL     string      `json:"eol"`
	Data    string      `json:"data"` // base64
	Kind    string      `json:"kind"` // lookup: host, ip4, ip6, addr, mx, txt, srv, cname, ns
	Name    string      `json:"name"`
	Service string      `json:"service"`
	Proto   string      `json:"proto"`
	Server  string      `json:"server"` // lookup: a DNS server ("host:port") instead of the system's
}

func init() {
	registerPHP("net.php")
	ops := map[string]func(context.Context, netRequest) map[string]any{
		"net.dial":         netDial,
		"net.listen":       netListen,
		"net.listenPacket": netListenPacket,
		"net.accept":       netAccept,
		"net.read":         netRead,
		"net.write":        netWrite,
		"net.readFrom":     netReadFrom,
		"net.writeTo":      netWriteTo,
		"net.close":        netClose,
		"net.lookup":       netLookup,
	}
	for name, fn := range ops {
		fn := fn
		core.RegisterOp(name, func(ctx context.Context, payload []byte) (any, error) {
			var req netRequest
			if err := wire.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			return fn(ctx, req), nil
		})
	}
}

func netFail(kind string, err error) map[string]any {
	return map[string]any{"ok": false, "kind": kind, "error": err.Error()}
}

// netErr classifies an I/O error for PHP: timeout, closed or io.
func netErr(err error) map[string]any {
	var ne net.Error
	switch {
	case errors.As(err, &ne) && ne.Timeout(), errors.Is(err, os.ErrDeadlineExceeded):
		return netFail("timeout", err)
	case errors.Is(err, net.ErrClosed):
		return netFail("closed", err)
	default:
		return netFail("io", err)
	}
}

func netTimeout(seconds float64) time.Duration {
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds * float64(time.Second))
}

// withDeadline runs fn with set(deadline) applied: the op's own timeout, cut
// short when ctx ends (the request's deadline or cancellation).
func withDeadline(ctx context.Context, timeout float64, set func(time.Time) error, fn func() error) error {
	var dl time.Time
	if d := netTimeout(timeout); d > 0 {
		dl = time.Now().Add(d)
	}
	_ = set(dl)
	stop := context.AfterFunc(ctx, func() { _ = set(time.Unix(1, 0)) })
	err := fn()
	stop()
	_ = set(time.Time{})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func addConn(c net.Conn) map[string]any {
	id := netNextID.Add(1)
	netConns.Store(id, &netConn{conn: c, r: bufio.NewReaderSize(c, 64*1024)})
	return map[string]any{"ok": true, "id": id, "local": c.LocalAddr().String(), "remote": c.RemoteAddr().String()}
}

func netDial(ctx context.Context, req netRequest) map[string]any {
	if d := netTimeout(req.Timeout); d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	network := req.Network
	if network == "tls" {
		host, _, err := net.SplitHostPort(req.Address)
		if err != nil {
			return netFail("dial", err)
		}
		o := tlsOptions{VerifyPeer: true, VerifyPeerName: true}
		if req.TLS != nil {
			o = *req.TLS
		}
		cfg, err := o.config(host)
		if err != nil {
			return netFail("tls", err)
		}
		c, err := (&tls.Dialer{Config: cfg}).DialContext(ctx, "tcp", req.Address)
		if err != nil {
			if ctx.Err() != nil {
				return netFail("timeout", err)
			}
			if strings.Contains(err.Error(), "tls:") || strings.Contains(err.Error(), "x509:") {
				return netFail("tls", err)
			}
			return netFail("dial", err)
		}
		return addConn(c)
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, network, req.Address)
	if err != nil {
		if ctx.Err() != nil {
			return netFail("timeout", err)
		}
		return netFail("dial", err)
	}
	return addConn(c)
}

func netListen(_ context.Context, req netRequest) map[string]any {
	network := req.Network
	var ln net.Listener
	var err error
	if network == "tls" {
		key := req.Key
		if key == "" {
			key = req.Cert
		}
		cert, cerr := tls.LoadX509KeyPair(req.Cert, key)
		if cerr != nil {
			return netFail("tls", cerr)
		}
		network = "tcp"
		ln, err = net.Listen(network, req.Address)
		if err == nil {
			raw := ln
			ln = tls.NewListener(raw, &tls.Config{Certificates: []tls.Certificate{cert}})
			id := netNextID.Add(1)
			netListeners.Store(id, &netListener{ln: ln, raw: raw.(deadliner)})
			return map[string]any{"ok": true, "id": id, "address": ln.Addr().String()}
		}
	} else {
		ln, err = net.Listen(network, req.Address)
	}
	if err != nil {
		return netFail("listen", err)
	}
	var dl deadliner = deadlineNop{}
	if d, ok := ln.(deadliner); ok {
		dl = d
	}
	id := netNextID.Add(1)
	netListeners.Store(id, &netListener{ln: ln, raw: dl})
	return map[string]any{"ok": true, "id": id, "address": ln.Addr().String()}
}

func netListenPacket(_ context.Context, req netRequest) map[string]any {
	pc, err := net.ListenPacket(req.Network, req.Address)
	if err != nil {
		return netFail("listen", err)
	}
	id := netNextID.Add(1)
	netPackets.Store(id, pc)
	return map[string]any{"ok": true, "id": id, "address": pc.LocalAddr().String()}
}

type deadliner interface{ SetDeadline(time.Time) error }

func netAccept(ctx context.Context, req netRequest) map[string]any {
	v, ok := netListeners.Load(req.ID)
	if !ok {
		return netFail("closed", net.ErrClosed)
	}
	l := v.(*netListener)
	var c net.Conn
	err := withDeadline(ctx, req.Timeout, l.raw.SetDeadline, func() error {
		var err error
		c, err = l.ln.Accept()
		return err
	})
	if err != nil {
		if ctx.Err() != nil {
			return map[string]any{"ctx": true}
		}
		return netErr(err)
	}
	return addConn(c)
}

type deadlineNop struct{}

func (deadlineNop) SetDeadline(time.Time) error { return nil }

func loadConn(id int64) (*netConn, bool) {
	v, ok := netConns.Load(id)
	if !ok {
		return nil, false
	}
	return v.(*netConn), true
}

// netRead reads up to max bytes ("some"), exactly max ("exact"), or up to and
// including eol, at most max bytes ("line"). EOF before any byte is
// {"eof": true}; "exact" and "line" return what they got with "eof" set
// when the peer closed early.
func netRead(ctx context.Context, req netRequest) map[string]any {
	nc, ok := loadConn(req.ID)
	if !ok {
		return netFail("closed", net.ErrClosed)
	}
	if req.Max <= 0 {
		req.Max = 64 * 1024
	}
	nc.rmu.Lock()
	defer nc.rmu.Unlock()
	var out []byte
	err := withDeadline(ctx, req.Timeout, nc.conn.SetReadDeadline, func() error {
		switch req.Mode {
		case "exact":
			out = make([]byte, req.Max)
			n, err := io.ReadFull(nc.r, out)
			out = out[:n]
			return err
		case "line":
			eol := []byte(req.EOL)
			if len(eol) == 0 {
				eol = []byte("\n")
			}
			for len(out) < req.Max {
				b, err := nc.r.ReadByte()
				if err != nil {
					return err
				}
				out = append(out, b)
				if bytes.HasSuffix(out, eol) {
					return nil
				}
			}
			return nil
		default:
			buf := make([]byte, req.Max)
			n, err := nc.r.Read(buf)
			out = buf[:n]
			if n > 0 {
				return nil
			}
			return err
		}
	})
	res := map[string]any{"ok": true, "data": base64.StdEncoding.EncodeToString(out)}
	switch {
	case err == nil:
	case ctx.Err() != nil:
		return map[string]any{"ctx": true}
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		res["eof"] = true
	default:
		if len(out) > 0 && req.Mode == "some" {
			break
		}
		return netErr(err)
	}
	return res
}

func netWrite(ctx context.Context, req netRequest) map[string]any {
	nc, ok := loadConn(req.ID)
	if !ok {
		return netFail("closed", net.ErrClosed)
	}
	data, err := base64.StdEncoding.DecodeString(req.Data)
	if err != nil {
		return netFail("io", err)
	}
	nc.wmu.Lock()
	defer nc.wmu.Unlock()
	var n int
	err = withDeadline(ctx, req.Timeout, nc.conn.SetWriteDeadline, func() error {
		var err error
		n, err = nc.conn.Write(data)
		return err
	})
	if err != nil {
		if ctx.Err() != nil {
			return map[string]any{"ctx": true}
		}
		return netErr(err)
	}
	return map[string]any{"ok": true, "written": n}
}

func netReadFrom(ctx context.Context, req netRequest) map[string]any {
	v, ok := netPackets.Load(req.ID)
	if !ok {
		return netFail("closed", net.ErrClosed)
	}
	pc := v.(net.PacketConn)
	if req.Max <= 0 {
		req.Max = 64 * 1024
	}
	buf := make([]byte, req.Max)
	var n int
	var from net.Addr
	err := withDeadline(ctx, req.Timeout, pc.SetReadDeadline, func() error {
		var err error
		n, from, err = pc.ReadFrom(buf)
		return err
	})
	if err != nil {
		if ctx.Err() != nil {
			return map[string]any{"ctx": true}
		}
		return netErr(err)
	}
	return map[string]any{"ok": true, "data": base64.StdEncoding.EncodeToString(buf[:n]), "from": from.String()}
}

func netWriteTo(ctx context.Context, req netRequest) map[string]any {
	v, ok := netPackets.Load(req.ID)
	if !ok {
		return netFail("closed", net.ErrClosed)
	}
	pc := v.(net.PacketConn)
	data, err := base64.StdEncoding.DecodeString(req.Data)
	if err != nil {
		return netFail("io", err)
	}
	addr, err := net.ResolveUDPAddr(pc.LocalAddr().Network(), req.Address)
	if err != nil {
		return netFail("io", err)
	}
	var n int
	err = withDeadline(ctx, req.Timeout, pc.SetWriteDeadline, func() error {
		var err error
		n, err = pc.WriteTo(data, addr)
		return err
	})
	if err != nil {
		if ctx.Err() != nil {
			return map[string]any{"ctx": true}
		}
		return netErr(err)
	}
	return map[string]any{"ok": true, "written": n}
}

// netClose closes a connection, listener or packet connection; mode "write"
// only shuts down the writing side of a connection.
func netClose(_ context.Context, req netRequest) map[string]any {
	if v, ok := netConns.Load(req.ID); ok {
		nc := v.(*netConn)
		if req.Mode == "write" {
			type closeWriter interface{ CloseWrite() error }
			if cw, ok := nc.conn.(closeWriter); ok {
				if err := cw.CloseWrite(); err != nil {
					return netErr(err)
				}
			}
			return map[string]any{"ok": true}
		}
		netConns.Delete(req.ID)
		nc.close.Do(func() { _ = nc.conn.Close() })
	}
	if v, ok := netListeners.LoadAndDelete(req.ID); ok {
		_ = v.(*netListener).ln.Close()
	}
	if v, ok := netPackets.LoadAndDelete(req.ID); ok {
		_ = v.(net.PacketConn).Close()
	}
	return map[string]any{"ok": true}
}

func netLookup(ctx context.Context, req netRequest) map[string]any {
	r := net.DefaultResolver
	if req.Server != "" {
		server := req.Server
		r = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, server)
		}}
	}
	if d := netTimeout(req.Timeout); d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	var out any
	var err error
	switch req.Kind {
	case "host":
		out, err = r.LookupHost(ctx, req.Name)
	case "ip4", "ip6", "ip":
		var ips []net.IP
		ips, err = r.LookupIP(ctx, req.Kind, req.Name)
		list := []string{}
		for _, ip := range ips {
			list = append(list, ip.String())
		}
		out = list
	case "addr":
		out, err = r.LookupAddr(ctx, req.Name)
	case "cname":
		out, err = r.LookupCNAME(ctx, req.Name)
	case "txt":
		out, err = r.LookupTXT(ctx, req.Name)
	case "mx":
		var mx []*net.MX
		mx, err = r.LookupMX(ctx, req.Name)
		list := []map[string]any{}
		for _, m := range mx {
			list = append(list, map[string]any{"host": m.Host, "pref": m.Pref})
		}
		out = list
	case "ns":
		var ns []*net.NS
		ns, err = r.LookupNS(ctx, req.Name)
		list := []string{}
		for _, n := range ns {
			list = append(list, n.Host)
		}
		out = list
	case "srv":
		var srv []*net.SRV
		var cname string
		cname, srv, err = r.LookupSRV(ctx, req.Service, req.Proto, req.Name)
		list := []map[string]any{}
		for _, s := range srv {
			list = append(list, map[string]any{"target": s.Target, "port": s.Port, "priority": s.Priority, "weight": s.Weight})
		}
		out = map[string]any{"cname": cname, "records": list}
	default:
		return netFail("lookup", errors.New("unknown lookup "+req.Kind))
	}
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return netFail("not_found", err)
		}
		if ctx.Err() != nil {
			return netFail("timeout", err)
		}
		return netFail("lookup", err)
	}
	return map[string]any{"ok": true, "result": out}
}

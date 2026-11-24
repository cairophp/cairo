package httpserver

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/cairophp/cairo/internal/duplex"
)

// A WebSocket that PHP serves in the fiber of its request
// (Cairo\Http\WebSocket). The handshake is an operation of PHP, so the
// handler has seen the request and agreed before the connection is one
// (OpenSocket). From then on the request's call stays open, as for a
// streamed answer, and the handler in Go ends the socket when PHP answers
// it. What PHP sends waits in a queue for one writer, each message put
// there by one direct call (TrySendSocket). What the client sends is read
// ahead of PHP, which takes it message by message without waiting
// (TakeSocket) and waits in an operation only when none has arrived.
//
// A client that leaves does not end its handler's call, as the client of
// a streamed answer does: what it sent before it left is still PHP's to
// take and to work on, and PHP learns of the end where it uses the socket
// (TakeSocket says over, a send fails).
//
// A message on its way, in either direction, has its type in front
// (Message): byte 0 is websocket.MessageText or websocket.MessageBinary.

// SocketOptions is what PHP asks of a handshake.
type SocketOptions struct {
	Origins    []string `json:"origins"`     // hosts that may connect from a page, besides the request's own
	Protocols  []string `json:"protocols"`   // the subprotocols to choose from
	MaxMessage int64    `json:"max_message"` // how many bytes a message of the client may have
	Ping       float64  `json:"ping"`        // seconds between pings; 0 for none
}

// What a message of the client may have when PHP names no limit.
const maxSocketMessage = 64 << 10

type socket struct {
	id    int64
	conn  *websocket.Conn
	sends *duplex.SendQueue
	reads *duplex.ReadAhead[[]byte]
	stall time.Duration // how long one write may take; 0 for no limit
	ctx   context.Context
	leave context.CancelFunc // ends the reader and the pings
	subscription

	mu    sync.Mutex
	taken [][]byte // read ahead and taken from there, not yet PHP's
	over  bool     // the client has gone, and PHP has all that it sent
}

var sockets sync.Map // int64 -> *socket

// refusal is the answer of a request whose handshake failed: the client
// has it, and PHP's is dropped.
type refusal struct{}

func (refusal) finish(bool) {}

// OpenSocket makes the connection of the request that ctx is an operation
// of a WebSocket, and returns the id under which PHP uses it with the
// subprotocol that was chosen. A handshake that fails is answered here (a
// request that asked for no WebSocket, a page of another origin): the id
// is 0 then, and that is not an error of PHP's.
func OpenSocket(ctx context.Context, opts SocketOptions, log *slog.Logger) (id int64, protocol string, err error) {
	ev, err := writerOf(ctx)
	if err != nil {
		return 0, "", err
	}
	defer ev.mu.Unlock()
	if ev.began != nil {
		return 0, "", errors.New("the answer has already begun")
	}
	// The server's timeouts were set for a request and its answer.
	rc := http.NewResponseController(ev.w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
	conn, err := accept(ev, opts)
	if err != nil {
		ev.began = refusal{}
		log.Info("websocket handshake refused", "route", ev.name, "remote", ev.remote, "err", err)
		return 0, "", nil
	}
	s := &socket{id: nextStreamID.Add(1), conn: conn, stall: ev.stall}
	s.ctx, s.leave = context.WithCancel(context.Background())
	s.sends = duplex.NewSendQueue(s.write, nil)
	s.reads = duplex.NewReadAhead(s.ctx, s.read)
	if every := seconds(opts.Ping); every > 0 {
		go s.keepAlive(every)
	}
	ev.began = s
	sockets.Store(s.id, s)
	return s.id, conn.Subprotocol(), nil
}

// accept answers the handshake. The library writes the refusal itself:
// 426 for a request that asked for no WebSocket, 403 for a foreign origin.
func accept(ev *event, opts SocketOptions) (*websocket.Conn, error) {
	conn, err := websocket.Accept(ev.w, ev.req, &websocket.AcceptOptions{
		Subprotocols:   opts.Protocols,
		OriginPatterns: opts.Origins,
	})
	if err != nil {
		return nil, err
	}
	limit := opts.MaxMessage
	if limit <= 0 {
		limit = maxSocketMessage
	}
	conn.SetReadLimit(limit)
	return conn, nil
}

// read is the next message of the client, its type in front.
func (s *socket) read() ([]byte, error) {
	typ, r, err := s.conn.Reader(s.ctx)
	if err != nil {
		return nil, err
	}
	msg := make([]byte, 1, 512)
	msg[0] = byte(typ)
	buf := bytes.NewBuffer(msg)
	if _, err := buf.ReadFrom(r); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (s *socket) write(msg []byte) error {
	ctx := s.ctx
	if s.stall > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.stall)
		defer cancel()
	}
	return s.conn.Write(ctx, websocket.MessageType(msg[0]), msg[1:])
}

// keepAlive pings the client until the socket ends.
func (s *socket) keepAlive(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-s.reads.Done():
			return
		case <-t.C:
			s.ping(every)
		}
	}
}

// ping drops a client that does not answer within wait. The reader is
// what sees the answer, and it rests while PHP has not taken what it
// holds: silence says nothing about the client then.
func (s *socket) ping(wait time.Duration) {
	ctx, cancel := context.WithTimeout(s.ctx, wait)
	defer cancel()
	if err := s.conn.Ping(ctx); err != nil && !s.reads.Full() {
		_ = s.conn.CloseNow()
	}
}

// Message returns data as a message on its way: its type in front.
func Message(binary bool, data []byte) []byte {
	msg := make([]byte, 1+len(data))
	msg[0] = byte(websocket.MessageText)
	if binary {
		msg[0] = byte(websocket.MessageBinary)
	}
	copy(msg[1:], data)
	return msg
}

func socketOf(id int64) (*socket, error) {
	v, ok := sockets.Load(id)
	if !ok {
		return nil, ErrStreamClosed
	}
	return v.(*socket), nil
}

// TrySendSocket queues msg (see Message) for the client of socket id if
// there is room, without waiting; queued is false when there is none and
// the caller should use SendSocket.
func TrySendSocket(id int64, msg []byte) (queued bool, err error) {
	s, err := socketOf(id)
	if err != nil {
		return false, err
	}
	return s.sends.TryPut(msg)
}

// SendSocket queues msg for the client of socket id, waiting for room
// until ctx ends: a client that reads slowly holds its handler back there.
func SendSocket(ctx context.Context, id int64, msg []byte) error {
	s, err := socketOf(id)
	if err != nil {
		return err
	}
	return s.sends.Put(ctx, msg)
}

// TakeSocket returns the next message that the client of socket id has
// sent, without waiting. over is true once the client has gone and PHP has
// all that it sent; with neither, WaitSocket waits for more.
func TakeSocket(id int64) (msg []byte, over bool) {
	s, err := socketOf(id)
	if err != nil {
		return nil, true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.taken) == 0 && !s.over {
		var end error
		s.taken, end = s.reads.TryTake()
		s.over = end != nil
	}
	if len(s.taken) == 0 {
		return nil, s.over
	}
	msg, s.taken = s.taken[0], s.taken[1:]
	return msg, false
}

// WaitSocket returns when the client of socket id has sent a message or
// has gone, or with ctx's error.
func WaitSocket(ctx context.Context, id int64) error {
	s, err := socketOf(id)
	if err != nil {
		return nil // over: TakeSocket says so
	}
	return s.reads.Wait(ctx)
}

// CloseSocket ends socket id with a status of PHP's choosing, once what
// PHP sent is written. It waits for the client to close in turn, a few
// seconds at most; a client that has left already, or does not answer, is
// no error of PHP's.
func CloseSocket(id int64, code int, reason string) {
	s, err := socketOf(id)
	if err != nil {
		return
	}
	if s.sends.Close() == nil {
		_ = s.conn.Close(websocket.StatusCode(code), reason)
	}
}

// finish ends the socket of a request whose call has returned: what PHP
// queued is written, and the client is told why it ends.
func (s *socket) finish(failed bool) {
	sockets.Delete(s.id)
	s.unsubscribe()
	code := websocket.StatusNormalClosure
	if failed {
		code = websocket.StatusInternalError
	}
	if s.sends.Close() == nil {
		_ = s.conn.Close(code, "") // nothing, after PHP's close() or when the client has gone
	}
	s.leave()
	_ = s.conn.CloseNow()
}

// stopSockets runs when the server begins to stop: every client is told
// to go away, and each handler learns that its client has gone.
func stopSockets() {
	sockets.Range(func(_, v any) bool {
		go func() { _ = v.(*socket).conn.Close(websocket.StatusGoingAway, "") }()
		return true
	})
}

// IsBinary reports whether msg, a message on its way, is a binary one.
func IsBinary(msg []byte) bool { return msg[0] == byte(websocket.MessageBinary) }

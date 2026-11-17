# Performance work log

What was changed to make Cairo faster, why, and what each change measured.
Every number comes from `bench/run.sh` (or a smaller run of the same cases)
on the machine described in [README.md](README.md): an i7-8565U laptop, the
server pinned to two physical cores, PHP 8.5.12 with opcache. Numbers on this
machine move by 5-10 % between runs, so a change is only kept when it shows
up beyond that.

## Results after each step

Cairo, requests per second (calls or streams per second for gRPC), median of
three 10 s runs. The other servers' numbers are in [README.md](README.md).

| Case | v0.1.0 | 1. first fixes | 2. MySQL + GC | 3. binary events |
|---|---:|---:|---:|---:|
| hello | 19,481 | 36,314 | 42,322 | **49,601** |
| json | 19,231 | 35,120 | 40,633 | **45,959** |
| cpu | 3,698 | 3,901 | 3,895 | 4,039 |
| sleep (20 ms) | 11,570 | 11,023 | 11,227 | 11,303 |
| curl (20 ms upstream) | 5,973 | 11,430 | 11,612 | 11,631 |
| `Cairo\Http\Client` | 8,043 | 10,631 | 11,351 | 11,346 |
| PDO PostgreSQL point query | 8,089 | 11,682 | 12,377 | **14,271** |
| PDO PostgreSQL 10 ms query | 3,630 | 3,567 | 3,620 | 3,650 |
| `Cairo\Sql` PostgreSQL | 9,023 | 13,875 | 15,374 | **16,621** |
| PDO MySQL point query | 7,131 | 12,881 | 13,461 | **15,659** |
| PDO MySQL 10 ms query | 3,722 | 3,679 | 3,666 | 3,728 |
| `Cairo\Sql` MySQL | 6,854 | 9,459 | 15,425 | **15,938** |
| gRPC unary | 12,979 | 15,075 | 15,926 | **16,485** |
| gRPC server streaming (100 replies) | 444 | 820 | 886 | **909** |
| gRPC 20 ms handler | 2,753 | 2,745 | 2,778 | 2,795 |

The sleep and 10 ms query cases do not move: they are bound by the wait
itself and by the 40 database connections every server gets (40 / 10 ms is
4,000 req/s at most). In v0.1.0 the PDO cases also returned errors (see 1e).

## 1. First fixes (commit d201bba)

Found by measuring per-thread CPU (`/proc/<pid>/task/*/stat`), context
switches and TLB shootdowns (`/proc/interrupts`) under load, and by
sweeping the thread count: on hello, one PHP thread did 29k req/s and four
did 22k, so something got worse with more threads.

### 1a. A Go scheduler slot for each PHP thread

**What.** `GOMAXPROCS` is set to the CPU count plus the number of PHP
threads, unless `GOMAXPROCS` is in the environment (`internal/sapi/engine.go`).

**Why.** A PHP thread is a C thread. Each time it calls into Go (to answer a
request, to start an I/O wait) it needs a free P, Go's scheduler slot. With
one P per CPU, net/http's goroutines held them all under load and the PHP
threads queued for one. Raising `GOMAXPROCS` from 4 to 8 by hand took hello
from 22.9k to 26.5k and halved its p99.

**Effect.** About +15 % on hello, p99 18 ms to 8-10 ms.

### 1b. Reuse request fibers

**What.** A fiber whose request finished is kept (up to 64 per thread) and
runs the next request or `async()` task (`Scheduler::startFiber()` in
`php/runtime.php`). The per-fiber deadline state in C is reset when the
handler's `cairo_guard()` ends, so a reused fiber starts clean
(`internal/sapi/cairo.c`).

**Why.** Every request created a Fiber: PHP mmaps its stack and munmaps it at
the end, and the munmap sends a TLB-shootdown interrupt to every CPU running
the process. Measured: 2.0 shootdowns per request, and creating a fiber cost
7.2 µs on its own.

**Effect.** Shootdowns per request 2.0 → 0.08. Together with 1c, hello went
from 23k to 34k with `GOMAXPROCS` pinned, before 1a's gain.

### 1c. Typed request events

**What.** The HTTP and gRPC servers built each request as a
`map[string]any` for `json.Marshal`; they now use structs.

**Why.** Encoding the map took 6.0 µs and 39 allocations per request, the
struct 1.4 µs and 8 (a Go benchmark of the same event). Replaced again in 3.

### 1d. curl keep-alive

**What.** The curl hook keeps one multi handle per easy handle, in a
per-thread table, and frees it in a hooked `curl_easy_cleanup()`
(`internal/sapi/curl_hook.c`, `scripts/build-static.sh`).

**Why.** A hooked `curl_exec()` ran the transfer on a new multi handle and
destroyed it after. A multi handle owns its connection cache, so every
`curl_exec()` opened a new TCP connection even on a reused handle: 239 µs
per call to a local server, against 123 µs through `Cairo\Http\Client`.

**Effect.** curl 6.1k → 11.2k req/s. Now ahead of Swoole (9.1k) on this case.

### 1e. `Cairo\Pool` drops connections of cancelled requests

**What.** `Pool::with()` drops the connection when the callback throws or the
request's context ended; a connection held with `get()` is dropped when the
fiber's context ended. `discard()` is documented for connection errors.

**Why.** A bug found while setting up the benchmark. When a client goes away,
Cairo cancels the request's query, which closes the PDO connection mid-reply,
and the pool handed the dead connection to the next request: every later
request on it failed with "server closed the connection" or "MySQL server has
gone away".

**Effect.** No more errors in the PDO cases (v0.1.0 had up to 53 per run).

### 1f. `Cairo\Sql` keeps its connections idle

**What.** `max_idle` defaults to `max_open` (10 when unlimited).

**Why.** It defaulted to 2, Go's default, so under load database/sql closed
almost every connection after use and opened a new one for the next query.

**Effect.** `Cairo\Sql` PostgreSQL at 64 connections: 950 → 11,400 req/s.

### 1g. gRPC stream replies are queued

**What.** `ServerStream::send()` hands the reply to a 64-entry queue that one
goroutine per stream writes (`internal/grpcserver/stream.go`), through a call
that does not suspend the fiber. A full queue falls back to waiting on the
fiber's goroutine. Replies are flushed before the call's status; a failed
write is thrown by the next `send()`.

**Why.** Each `send()` was a full round trip: JSON op, fiber suspend, the
goroutine writes, an event back, the fiber resumes.

**Effect.** Server streaming 480 → 836 streams/s (100 replies each).

### 1h. No stack trace for cancelled requests

**What.** A request cancelled by its client answers 499 without logging.

**Why.** Every client that went away logged a full stack trace at INFO.

## 2. MySQL round trips and Go's GC (commit 0e06390)

### 2a. `Cairo\Sql` MySQL interpolates parameters

**What.** MySQL URLs get `interpolateParams=true` unless they set it.

**Why.** go-sql-driver/mysql ran every parameterised query as a server-side
prepare, execute and close: three round trips, against one for PostgreSQL
(pgx caches its statements). The driver escapes for the connection's charset
and refuses the charsets where that is unsafe.

**Effect.** `Cairo\Sql` MySQL 10.4k → 14.5k in a direct comparison.

### 2b. `GOGC=400` by default

**What.** `debug.SetGCPercent(400)` unless `GOGC` is set.

**Why.** Go's live heap is small (request data lives in PHP), so at the
default of 100 the collector ran every few hundred requests. Measured on
hello: 100 → 43.5k, 200 → 44.0k, 400 → 45.3k, 800 → 45.9k req/s, for 61,
66, 73 and 89 MiB of RSS. 400 is where the gain flattens.

**Effect.** About +10 % on hello for about 12 MiB.

## 3. Binary events and raw bodies (commit d3bacd0)

**What.** Events from Go to PHP (requests and I/O results) are encoded in a
small tagged binary format (`internal/wire`) and decoded by C straight into
PHP arrays (`internal/sapi/wire.c`). Values without a direct encoding still
go as JSON and are decoded by PHP's json extension. Request and response
bodies cross as raw bytes: `cairo_respond()` takes the body as its own
argument. PHP → Go metadata (status, headers, op payloads) stays JSON: it is
small and Go decodes it into typed structs.

**Why.** Each request was JSON text that PHP decoded with `json_decode()`,
with the body base64'd inside it, and the response came back the same way.
The other servers all pass bodies raw: FrankenPHP reads PHP's output through
SAPI callbacks, RoadRunner puts the body after the JSON header in its frame,
FastCGI has raw STDIN/STDOUT records, and Swoole never leaves C.

**Options weighed.** msgpack needs an extra PHP extension and a
reflection-based Go encoder, for no speed over this. Building PHP values from
Go-owned C structs, as FrankenPHP does, needs C per event type. Keeping JSON
and moving only bodies out gets about half the gain.

**Safety.** The decoder checks every length against what is left of the
buffer and limits nesting to 64; a Go mirror of it is fuzzed
(`internal/wire`). Testing found that PHP's JSON scanner reads until a NUL
byte, so JSON values get their own terminated copy instead of a slice of the
buffer.

**Effect.** Encoding a request: 141 ns and no allocations, against 1.4 µs for
`encoding/json`. hello 46.1k → 50.9k and a 64 KiB echo 938 → 3,638 req/s
side by side; in the full run, hello 42.3k → 49.6k and the PostgreSQL and
MySQL point queries +15 %.

## Tried and not kept

| Change | Result | Branch |
|---|---|---|
| PHP threads spin before waiting for an event | Worse (hello 21.9k → 19.2k): a spinning thread inside a Go callback holds a P that net/http needs | not kept |
| The socket poller wakes the PHP thread directly, skipping the fiber's goroutine | No difference: that goroutine wake runs on the same P and costs no thread switch | not kept |
| PHP threads wait for events in a C queue (inbox.c) instead of inside Go | No difference: hello 49k either way, 76-80 µs CPU and 2.1 context switches per request | `experiment-c-inbox` |
| msgpack instead of the binary format | Not built: slower to decode in PHP without an extension | – |

## Where the time goes now

On hello, Cairo spends about 65 µs of CPU per request with one PHP thread:
about 17 µs in PHP (Cairo's runtime: Request, Context, the response) and
about 48 µs in Go, of which plain net/http is about 25 µs. Swoole spends
about 33 µs in total: its HTTP server is C on the same thread as PHP.

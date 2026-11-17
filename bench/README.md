# Benchmarks

Cairo against the other ways to serve PHP: nginx + PHP-FPM, FrankenPHP in
worker mode, RoadRunner and Swoole, with plain Go net/http as a ceiling. Every
server runs the same PHP (`apps/workload.php`) on PHP 8.5.12 with opcache on and
JIT off; only the way each one runs it differs.

```sh
make static                                   # dist/cairo
PATH=/path/to/wrk-and-ghz:$PATH bench/run.sh  # or: bench/run.sh cairo swoole
WORKERS=64 CASES="sleep curl" bench/run.sh fpm frankenphp roadrunner   # other settings
```

`run.sh` needs Docker, `wrk`, `ghz`, Go, and the `cairo-pg` / `cairo-mysql`
containers from AGENTS.md. It seeds a 1000-row `bench_items` table in both,
builds the competitor images (`apps/*/Dockerfile`), and writes
`results/raw.csv` (every run) and `results/results.md` (medians).

[IMPROVEMENTS.md](IMPROVEMENTS.md) lists each performance change made from
these benchmarks, why, and Cairo's numbers after each one.

## Cases

| Case | What one request does | wrk connections |
|---|---|---:|
| hello | return `Hello, World!` | 64 |
| json | `json_encode` a small array | 64 |
| cpu | recursive `fib(20)` in PHP, about 1 ms | 64 |
| sleep | `usleep(20000)` | 256 |
| curl | curl GET to a Go upstream that answers after 20 ms | 256 |
| client | the same through `Cairo\Http\Client` (Cairo only) | 256 |
| pgsql, mysql | PDO prepared `SELECT ... WHERE id = ?` on a random row | 64 |
| pgsql-slow, mysql-slow | PDO query that takes 10 ms in the database | 256 |
| sql-pgsql, sql-mysql | the point query through `Cairo\Sql` (Cairo only) | 64 |
| grpc-unary | `SayHello`, ghz, 4 connections, 64 concurrent | – |
| grpc-stream-100 | `CountUp` server streaming 100 replies (Cairo only) | – |
| grpc-slow-20ms | `SlowHello`, a 20 ms `Cairo\sleep()` (Cairo only) | – |

## Setup

| Server | Version | Concurrency model and config |
|---|---|---|
| Cairo | static `dist/cairo`, PHP 8.5.12 | 4 PHP threads, a fiber per request; PDO through `Cairo\Pool` (10 per thread), curl handles pooled |
| nginx + PHP-FPM | nginx 1.29, php:8.5.12-fpm | `pm = static`, 40 children, persistent PDO, nginx 4 workers over FastCGI TCP |
| FrankenPHP | 1.13.5, PHP 8.5.12 | worker mode, 40 workers |
| RoadRunner | 2025.1.15, php:8.5.12-cli | 40 HTTP and 40 gRPC workers, PSR-7 (nyholm), ext-protobuf |
| Swoole | 6.2.5, PHP 8.5.12 | `SWOOLE_BASE`, 4 workers, coroutines with `SWOOLE_HOOK_ALL`, `Swoole\ConnectionPool` (10 per worker) |
| Go | net/http | `upstream/main.go` `/hello` |

Every server gets the same database budget: 40 connections to each database.
The one-request-per-worker servers (FPM, FrankenPHP, RoadRunner) get 40
workers, one connection each; Cairo and Swoole get 4 threads or processes
sharing 40 connections (10 each). `Cairo\Sql` opens its own pool of 40, used
only by the `sql-*` cases.

Machine: Intel i7-8565U laptop (4 cores, 8 threads, powersave governor),
30 GB RAM, Ubuntu 26.04, kernel 7.0, Docker 29 with host networking. The
server is pinned to two physical cores (CPUs 0,1,4,5), wrk/ghz to CPUs 2,3,6
and the upstream to CPU 7; the databases are not pinned. Each case runs 3 s
of warm-up, then 3 runs of 10 s; tables show the median.

## Results (2026-11-17, Cairo with the perf-fixes branch)

Requests per second, median of 3 runs, p99 latency in brackets. Cairo before
the fixes (v0.1.0, other servers on 16 workers) is in `results-v0.1.0/`.

| Case | Go net/http | Cairo | nginx + PHP-FPM | FrankenPHP | RoadRunner | Swoole |
|---|---:|---:|---:|---:|---:|---:|
| hello | 99,085 (3.1 ms) | 49,601 (6.9 ms) | 13,798 (18.6 ms) | 18,144 (9.6 ms) | 13,604 (14.0 ms) | 121,563 (1.3 ms) |
| json | – | 45,959 (6.7 ms) | 11,248 (16.8 ms) | 16,897 (10.3 ms) | 13,443 (14.2 ms) | 119,565 (1.3 ms) |
| cpu | – | 4,039 (21.4 ms) | 3,146 (43.2 ms) | 3,174 (37.0 ms) | 3,808 (32.4 ms) | 3,801 (40.1 ms) |
| sleep | – | 11,303 (36.1 ms) | 1,921 (140.1 ms) | 1,967 (139.3 ms) | 1,901 (142.5 ms) | 11,981 (23.6 ms) |
| curl | – | 11,631 (44.4 ms) | 1,781 (160.9 ms) | 1,834 (151.9 ms) | 1,773 (156.0 ms) | 9,113 (49.8 ms) |
| client | – | 11,346 (46.4 ms) | – | – | – | – |
| pgsql | – | 14,271 (9.9 ms) | 6,357 (16.9 ms) | 7,622 (19.0 ms) | 6,367 (20.2 ms) | 9,246 (21.4 ms) |
| pgsql-slow | – | 3,650 (163.3 ms) | 3,643 (106.1 ms) | 3,750 (79.8 ms) | 3,528 (108.7 ms) | 3,607 (114.0 ms) |
| sql-pgsql | – | 16,621 (9.7 ms) | – | – | – | – |
| mysql | – | 15,659 (9.1 ms) | 7,901 (14.2 ms) | 9,610 (16.0 ms) | 9,014 (16.2 ms) | 24,803 (5.7 ms) |
| mysql-slow | – | 3,728 (80.5 ms) | 3,632 (103.0 ms) | 3,764 (92.1 ms) | 3,522 (93.8 ms) | 3,734 (92.2 ms) |
| sql-mysql | – | 15,938 (9.7 ms) | – | – | – | – |
| grpc-unary | – | 16,485 (8.6 ms) | – | – | 15,430 (9.8 ms) | – |
| grpc-stream-100 | – | 909 streams (151.4 ms) | – | – | – | – |
| grpc-slow-20ms | – | 2,795 (29.0 ms) | – | – | – | – |

Resident memory after the sleep case (MiB): Cairo 100, FPM + nginx 50,
FrankenPHP 96, RoadRunner 975, Swoole 26.

Cairo before and after the fixes, same machine: hello 19,481 → 49,601, json
19,231 → 45,959, curl 5,973 → 11,631, PostgreSQL point query 8,089 → 14,271,
MySQL point query 7,131 → 15,659, `Cairo\Sql` PostgreSQL 9,023 → 16,621 and
MySQL 6,854 → 15,938, gRPC unary 12,979 → 16,485, gRPC streaming 444 → 909
streams/s.

## Reading the numbers

- Requests that wait (sleep, curl): Cairo and Swoole hold hundreds of them
  per thread. Servers with one request per worker are capped at
  workers / latency (40 / 20 ms = 2,000 req/s), and more workers raise the
  cap at the cost of memory.
- The slow-query cases measure the connection budget, not the server: 40
  connections at 10 ms allow at most 4,000 req/s, and every server reaches
  about 3,600.
- Swoole is about 2.5x Cairo on hello and json, and ahead of plain Go: its HTTP
  server is C inside the PHP process. Cairo pays for Go's net/http (Go alone
  does 99k here) plus the handoff to a PHP thread.
- Swoole is 1.6x Cairo on the MySQL point query: its mysqlnd hooks run in C in
  its reactor, while each Cairo socket wait is a round trip through Go.
- CPU-bound work is the same everywhere: it is PHP's speed on two cores.

## Caveats

- One laptop with a desktop session and the browser running, so numbers move
  by about 5-10 % between runs; the 3-run median smooths some of it.
- The server shares physical cores with nothing else, but the load generator
  and databases share the machine; at 100k+ req/s wrk itself is close to its
  limit.
- Cairo runs on the host and the others in Docker with `--network host`; the
  network path is the same.
- gRPC is Cairo against RoadRunner only, and streaming is Cairo alone:
  RoadRunner's gRPC plugin serves unary calls only, and FrankenPHP, FPM and
  Swoole have no gRPC server without extra libraries.
- Each run ends with wrk closing connections mid-request. Cairo cancels
  those requests, and `Cairo\Pool` drops the connections they held; the next
  request opens a new one. gRPC calls cut off when ghz stops are not counted
  as errors (RoadRunner's few remaining ones are the same cut-off, reported
  differently).

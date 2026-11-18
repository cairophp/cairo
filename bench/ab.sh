#!/usr/bin/env bash
# Compares two Cairo binaries on the same machine state: the two take turns,
# ROUNDS times per case, so drift (heat, background load) hits both alike.
# Prints the median requests per second of each and the change.
#
#   bench/ab.sh old/cairo new/cairo                 # hello json pgsql mysql curl
#   CASES="hello mysql" ROUNDS=5 bench/ab.sh a b
#
# Needs wrk, Go, and the databases seeded by run.sh.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
A=$1
B=$2
WRK=${WRK:-wrk}
CASES=${CASES:-hello json pgsql mysql curl}
ROUNDS=${ROUNDS:-3}
DURATION=${DURATION:-6}
THREADS=${THREADS:-4}
SERVER_CPUS=${SERVER_CPUS:-0,1,4,5}
LOAD_CPUS=${LOAD_CPUS:-2,3,6}
UPSTREAM_CPUS=${UPSTREAM_CPUS:-7}

declare -A conns=([hello]=64 [json]=64 [cpu]=64 [sleep]=256 [curl]=256 [client]=256 [pgsql]=64
    [pgsql-slow]=256 [sql-pgsql]=64 [mysql]=64 [mysql-slow]=256 [sql-mysql]=64)

work=$(mktemp -d)
trap 'kill ${server_pid:-} ${upstream_pid:-} 2> /dev/null || true; rm -rf "$work"' EXIT
go build -o "$work/upstream" "$here/upstream"
taskset -c "$UPSTREAM_CPUS" "$work/upstream" -addr :9100 > /dev/null 2>&1 &
upstream_pid=$!

run() {   # binary path -> requests per second
    BENCH_THREADS=$THREADS taskset -c "$SERVER_CPUS" "$1" serve "$here/apps/cairo/server.php" > /dev/null 2>&1 &
    server_pid=$!
    for _ in $(seq 1 40); do
        curl -sf "http://127.0.0.1:8080/hello" > /dev/null && break
        sleep 0.25
    done
    curl -s --retry 3 --retry-all-errors --retry-delay 0 "http://127.0.0.1:8080$2" > /dev/null || true
    taskset -c "$LOAD_CPUS" "$WRK" -t3 -c "${conns[${2#/}]}" -d "${DURATION}s" "http://127.0.0.1:8080$2" |
        awk '/^Requests\/sec/ { print $2 }'
    kill "$server_pid"
    wait "$server_pid" 2> /dev/null || true
}

median() { sort -n | awk '{ v[NR] = $1 } END { print (NR % 2) ? v[(NR + 1) / 2] : (v[NR / 2] + v[NR / 2 + 1]) / 2 }'; }

printf '%-12s %10s %10s %8s\n' case A B change
for c in $CASES; do
    : > "$work/a"
    : > "$work/b"
    for _ in $(seq 1 "$ROUNDS"); do
        run "$A" "/$c" >> "$work/a"
        run "$B" "/$c" >> "$work/b"
    done
    a=$(median < "$work/a")
    b=$(median < "$work/b")
    printf '%-12s %10.0f %10.0f %+7.1f%%\n' "$c" "$a" "$b" "$(awk -v a="$a" -v b="$b" 'BEGIN { print (b - a) / a * 100 }')"
done

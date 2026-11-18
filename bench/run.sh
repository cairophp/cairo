#!/usr/bin/env bash
# Runs the benchmarks in bench/README.md and writes results/raw.csv.
#
#   bench/run.sh                     # every server
#   bench/run.sh cairo fpm           # some of them
#
# Needs: docker, wrk, ghz, go, curl, and the cairo-pg / cairo-mysql
# containers from AGENTS.md. CAIRO_BIN points at the binary under test.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/.." && pwd)

CAIRO_BIN=${CAIRO_BIN:-$repo/dist/cairo}
WRK=${WRK:-wrk}
GHZ=${GHZ:-ghz}
DURATION=${DURATION:-10}          # seconds per measured run
WARMUP=${WARMUP:-3}               # seconds of unmeasured load first
RUNS=${RUNS:-3}                   # measured runs per case; the median is reported
WORKERS=${WORKERS:-40}            # workers for one-request-per-worker servers (one DB connection each)
THREADS=${THREADS:-4}             # threads/processes for Cairo and Swoole
DB_CONNECTIONS=${DB_CONNECTIONS:-40}   # DB connections for Cairo and Swoole: the same as WORKERS
SERVER_CPUS=${SERVER_CPUS:-0,1,4,5}   # two physical cores with their SMT siblings
LOAD_CPUS=${LOAD_CPUS:-2,3,6}
UPSTREAM_CPUS=${UPSTREAM_CPUS:-7}
OUT=${OUT:-$here/results}
CASE_FILTER=${CASES:-}             # e.g. CASES="sleep curl" runs only those
SERVERS=("$@")
[ ${#SERVERS[@]} -gt 0 ] || SERVERS=(go cairo fpm frankenphp roadrunner swoole)

# name connections path [servers that have it]
ALL_CASES=(
    "hello 64 /hello"
    "json 64 /json"
    "cpu 64 /cpu"
    "cpu-cached 64 /cpu-cached cairo"
    "sleep 256 /sleep"
    "curl 256 /curl"
    "client 256 /client cairo"
    "pgsql 64 /pgsql"
    "pgsql-slow 256 /pgsql-slow"
    "sql-pgsql 64 /sql-pgsql cairo"
    "mysql 64 /mysql"
    "mysql-slow 256 /mysql-slow"
    "mysql-slow-cached 256 /mysql-slow-cached cairo"
    "sql-mysql 64 /sql-mysql cairo"
)

mkdir -p "$OUT" "$OUT/logs"
raw=$OUT/raw.csv
[ -f "$raw" ] || echo "server,case,connections,run,rps,p50_ms,p99_ms,errors" > "$raw"

log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }

# wrk prints latencies as 1.23ms, 456.78us or 1.02s.
to_ms() {
    awk -v v="$1" 'BEGIN {
        if (v ~ /us$/) { sub(/us$/, "", v); printf "%.3f", v / 1000 }
        else if (v ~ /ms$/) { sub(/ms$/, "", v); printf "%.3f", v }
        else if (v ~ /m$/) { sub(/m$/, "", v); printf "%.3f", v * 60000 }
        else { sub(/s$/, "", v); printf "%.3f", v * 1000 }
    }'
}

setup_databases() {
    log "seeding bench_items in PostgreSQL and MariaDB"
    docker exec -i cairo-pg psql -q -U cairo -d cairo <<'SQL'
DROP TABLE IF EXISTS bench_items;
CREATE TABLE bench_items (id int PRIMARY KEY, name text NOT NULL);
INSERT INTO bench_items SELECT i, 'item ' || i FROM generate_series(1, 1000) AS i;
SQL
    docker exec -i cairo-mysql mariadb -uroot -pcairo <<'SQL'
CREATE DATABASE IF NOT EXISTS bench;
USE bench;
DROP TABLE IF EXISTS bench_items;
CREATE TABLE bench_items (id int PRIMARY KEY, name varchar(64) NOT NULL);
INSERT INTO bench_items SELECT seq, CONCAT('item ', seq) FROM seq_1_to_1000;
SQL
}

build_images() {
    for s in "${SERVERS[@]}"; do
        case $s in fpm|frankenphp|roadrunner|swoole)
            log "building bench-$s"
            docker build -q -t "bench-$s" -f "$here/apps/$s/Dockerfile" "$here/apps" > /dev/null ;;
        esac
    done
}

start_upstream() {
    go build -o "$OUT/upstream" "$here/upstream"
    taskset -c "$UPSTREAM_CPUS" "$OUT/upstream" -addr :9100 > "$OUT/logs/upstream.log" 2>&1 &
    upstream_pid=$!
}

docker_run() {
    local name=$1; shift
    docker run -d --rm --name "$name" --network host --cpuset-cpus "$SERVER_CPUS" \
        -e BENCH_WORKERS="$WORKERS" -e BENCH_THREADS="$THREADS" -e BENCH_DB_CONNECTIONS="$DB_CONNECTIONS" \
        "$@" > /dev/null
}

start_server() {
    case $1 in
        go)
            taskset -c "$SERVER_CPUS" "$OUT/upstream" -addr :8080 > "$OUT/logs/go.log" 2>&1 &
            server_pid=$! ;;
        cairo)
            BENCH_THREADS=$THREADS BENCH_DB_CONNECTIONS=$DB_CONNECTIONS \
                taskset -c "$SERVER_CPUS" "$CAIRO_BIN" serve "$here/apps/cairo/server.php" > "$OUT/logs/cairo.log" 2>&1 &
            server_pid=$! ;;
        fpm)
            docker_run bench-fpm bench-fpm
            docker_run bench-nginx -v "$here/apps/fpm/nginx.conf:/etc/nginx/nginx.conf:ro" nginx:1.29-alpine ;;
        frankenphp) docker_run bench-frankenphp bench-frankenphp ;;
        roadrunner) docker_run bench-roadrunner bench-roadrunner ;;
        swoole) docker_run bench-swoole bench-swoole ;;
    esac
    for _ in $(seq 1 60); do
        if curl -sf http://127.0.0.1:8080/hello > /dev/null; then
            return 0
        fi
        sleep 0.5
    done
    log "$1 did not start"; return 1
}

stop_server() {
    case $1 in
        go|cairo) kill "$server_pid"; wait "$server_pid" 2> /dev/null || true ;;
        fpm) docker logs bench-fpm > "$OUT/logs/fpm.log" 2>&1; docker stop -t 2 bench-fpm bench-nginx > /dev/null ;;
        *) docker logs "bench-$1" > "$OUT/logs/$1.log" 2>&1; docker stop -t 2 "bench-$1" > /dev/null ;;
    esac
    sleep 2
}

# Resident memory of the server in MiB: the process tree, or the containers.
server_memory() {
    case $1 in
        go|cairo) ps -o rss= --pid "$server_pid" --ppid "$server_pid" | awk '{ s += $1 } END { printf "%.0f", s / 1024 }' ;;
        fpm) docker stats --no-stream --format '{{.MemUsage}}' bench-fpm bench-nginx | awk '{ print $1 }' | numfmt --from=iec-i --suffix=B 2> /dev/null | awk '{ s += $1 } END { printf "%.0f", s / 1048576 }' ;;
        *) docker stats --no-stream --format '{{.MemUsage}}' "bench-$1" | awk '{ print $1 }' | numfmt --from=iec-i --suffix=B 2> /dev/null | awk '{ s += $1 } END { printf "%.0f", s / 1048576 }' ;;
    esac
}

wrk_run() {   # connections seconds path
    taskset -c "$LOAD_CPUS" "$WRK" -t3 -c "$1" -d "${2}s" --timeout 10s --latency "http://127.0.0.1:8080$3"
}

run_http() {
    local server=$1
    for c in "${ALL_CASES[@]}"; do
        read -r name conns path only <<< "$c"
        if [ -n "$CASE_FILTER" ] && [[ " $CASE_FILTER " != *" $name "* ]]; then continue; fi
        if [ -n "${only:-}" ] && [ "$only" != "$server" ]; then continue; fi
        if [ "$server" = go ] && [ "$name" != hello ]; then continue; fi
        # A pooled connection broken by the last run's cut-off requests may
        # fail one request, so try a few times.
        if ! curl -sf --retry 3 --retry-all-errors --retry-delay 0 "http://127.0.0.1:8080$path" > /dev/null; then
            log "$server $name: not served, skipped"; continue
        fi
        wrk_run "$conns" "$WARMUP" "$path" > /dev/null
        for run in $(seq 1 "$RUNS"); do
            out=$(wrk_run "$conns" "$DURATION" "$path")
            echo "$out" > "$OUT/logs/wrk-$server-$name-$run.txt"
            rps=$(awk '/^Requests\/sec/ { print $2 }' <<< "$out")
            p50=$(to_ms "$(awk '$1 == "50%" { print $2 }' <<< "$out")")
            p99=$(to_ms "$(awk '$1 == "99%" { print $2 }' <<< "$out")")
            errors=$(awk '/Non-2xx/ { n += $NF } /Socket errors/ { gsub(/,/, ""); n += $4 + $6 + $8 + $10 } END { print n + 0 }' <<< "$out")
            echo "$server,$name,$conns,$run,$rps,$p50,$p99,$errors" >> "$raw"
            log "$server $name run $run: $rps req/s, p99 ${p99} ms, $errors errors"
        done
        if [ "$name" = sleep ]; then
            echo "$server,memory-after-sleep,,,$(server_memory "$server"),,," >> "$raw"
        fi
    done
}

ghz_run() {   # call proto data connections concurrency seconds
    taskset -c "$LOAD_CPUS" "$GHZ" --insecure --proto "$2" --call "$1" -d "$3" \
        --connections "$4" -c "$5" -z "${6}s" --format json 127.0.0.1:9000
}

run_grpc() {
    local server=$1 proto
    case $server in
        cairo) proto=$repo/examples/greeter/proto/helloworld.proto ;;
        roadrunner) proto=$here/apps/roadrunner/greeter.proto ;;
        *) return 0 ;;
    esac
    if [ -n "$CASE_FILTER" ] && [[ " $CASE_FILTER " != *" grpc "* ]]; then return 0; fi
    local cases=("unary helloworld.Greeter.SayHello {\"name\":\"bench\"}")
    if [ "$server" = cairo ]; then
        cases+=("stream-100 helloworld.Greeter.CountUp {\"name\":\"bench\",\"count\":100}")
        cases+=("slow-20ms helloworld.Greeter.SlowHello {\"name\":\"bench\",\"seconds\":0.02}")
    fi
    for c in "${cases[@]}"; do
        read -r name call data <<< "$c"
        ghz_run "$call" "$proto" "$data" 4 64 "$WARMUP" > /dev/null
        for run in $(seq 1 "$RUNS"); do
            out=$(ghz_run "$call" "$proto" "$data" 4 64 "$DURATION")
            echo "$out" > "$OUT/logs/ghz-$server-$name-$run.json"
            read -r rps p50 p99 errors <<< "$(python3 "$here/ghz_summary.py" <<< "$out")"
            echo "$server,grpc-$name,64,$run,$rps,$p50,$p99,$errors" >> "$raw"
            log "$server grpc-$name run $run: $rps calls/s, p99 ${p99} ms, $errors errors"
        done
    done
}

trap 'kill ${upstream_pid:-} ${server_pid:-} 2> /dev/null || true; docker stop bench-fpm bench-nginx bench-frankenphp bench-roadrunner bench-swoole > /dev/null 2>&1 || true' EXIT

setup_databases
build_images
start_upstream
for s in "${SERVERS[@]}"; do
    log "== $s"
    start_server "$s"
    curl -s http://127.0.0.1:8080/info > "$OUT/logs/info-$s.json" || true
    run_http "$s"
    run_grpc "$s"
    stop_server "$s"
done
python3 "$here/report.py" "$raw" > "$OUT/results.md"
log "wrote $OUT/results.md"

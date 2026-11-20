<?php
/**
 * Cairo\Sql: database connection pools that live in Go and are shared by
 * all PHP threads.
 */

declare(strict_types=1);

namespace Cairo\Sql {

    use Cairo\Internal\Go;

    /**
     * A SQL query failed. $sqlState is the five-character SQLSTATE code
     * (e.g. 23505 for a unique violation), when the database sent one.
     */
    class SqlException extends \RuntimeException
    {
        public function __construct(string $message, public readonly ?string $sqlState = null, public readonly ?string $detail = null)
        {
            parent::__construct($message);
        }
    }

    /** A binary parameter (bytea, BLOB). Values read back from binary columns are plain strings. */
    final class Bytes
    {
        public function __construct(public readonly string $bytes)
        {
        }
    }

    /** What Db and Tx share: running statements. */
    abstract class Queryable
    {
        /** @return array<string, int> which pool or transaction runs the statement */
        abstract protected function target(): array;

        /**
         * Run a query and return every row as column => value. Parameters
         * are $1, $2, ... for PostgreSQL and ? for MySQL. Only the calling
         * fiber waits.
         *
         * @param list<mixed> $params
         * @return list<array<string, mixed>>
         */
        public function query(string $sql, array $params = []): array
        {
            $res = $this->run($sql, $params, false);
            $columns = $res['columns'];
            $rows = [];
            foreach ($res['rows'] as $row) {
                $rows[] = array_combine($columns, array_map(self::decode(...), $row));
            }
            return $rows;
        }

        /**
         * The first row, or null when there is none.
         *
         * @param list<mixed> $params
         * @return array<string, mixed>|null
         */
        public function queryRow(string $sql, array $params = []): ?array
        {
            return $this->query($sql, $params)[0] ?? null;
        }

        /**
         * The first column of the first row, or null when there is no row.
         *
         * @param list<mixed> $params
         */
        public function queryValue(string $sql, array $params = []): mixed
        {
            $row = $this->queryRow($sql, $params);
            return $row === null ? null : reset($row);
        }

        /**
         * Run a statement and return how many rows it changed.
         *
         * @param list<mixed> $params
         */
        public function exec(string $sql, array $params = []): int
        {
            return (int) $this->run($sql, $params, true)['affected'];
        }

        /** @param list<mixed> $params */
        private function run(string $sql, array $params, bool $exec): array
        {
            $args = [];
            foreach (array_values($params) as $i => $p) {
                $args[] = match (true) {
                    $p instanceof Bytes => ['$bytes' => base64_encode($p->bytes)],
                    $p instanceof \DateTimeInterface => ['$time' => $p->format('Y-m-d\TH:i:s.uP')],
                    $p instanceof \BackedEnum => $p->value,
                    $p instanceof \Stringable => (string) $p,
                    is_array($p) || is_object($p) => throw new \InvalidArgumentException(sprintf('parameter %d: %s is not a SQL value; json_encode() it', $i + 1, get_debug_type($p))),
                    default => $p,
                };
            }
            if (\Cairo\Internal\QueryWatch::$on) {
                \Cairo\Internal\Queries::seen($sql);
            }
            return Db::call('sql.query', $this->target() + ['sql' => $sql, 'args' => $args, 'exec' => $exec]);
        }

        private static function decode(mixed $v): mixed
        {
            return is_array($v) && isset($v['$bytes']) ? base64_decode($v['$bytes']) : $v;
        }
    }

    /**
     * A PostgreSQL or MySQL connection pool in Go (database/sql with pgx or
     * go-sql-driver/mysql), shared by every PHP thread: Db::open() with the
     * same DSN and options returns the same pool on each thread, so max_open
     * bounds the connections of the whole server.
     *
     *     $db = Db::open('postgres://app:secret@127.0.0.1/app', ['max_open' => 20]);
     *     $users = $db->query('SELECT id, name FROM users WHERE team = $1', [$team]);
     *
     *     $db = Db::open('mysql://app:secret@127.0.0.1:3306/app');
     *     $users = $db->query('SELECT id, name FROM users WHERE team = ?', [$team]);
     *
     * Integers, floats, booleans, strings and nulls come back as such; binary
     * columns as strings, timestamps as RFC 3339 in UTC (MySQL DATE as
     * Y-m-d), numeric/DECIMAL, json and the rest in the database's text form.
     */
    final class Db extends Queryable
    {
        private function __construct(private readonly int $pool)
        {
        }

        /**
         * $dsn is a postgres:// URL, a PostgreSQL key=value DSN, or
         * mysql://user:pass@host:port/db (?socket=/path for a Unix socket,
         * plus the go-sql-driver/mysql parameters such as tls or timeout).
         *
         * Options: max_open (connections, 0 for no limit; default 10),
         * max_idle (default max_open, or 10 with no limit), max_lifetime and
         * max_idle_time (seconds). Fewer idle than open connections means a
         * reconnect for most queries under load; max_idle_time closes the
         * ones a quiet pool does not need. acquire_timeout (seconds) bounds
         * the wait for a free connection with a Cairo\PoolTimeout; without
         * it a query waits until its request ends. Connecting is lazy: a bad
         * DSN shows on the first query.
         *
         * Statements that would leave a transaction or a lock on a pooled
         * connection (BEGIN, START TRANSACTION, SET autocommit, LOCK, XA)
         * throw a SqlException: use transaction() instead.
         *
         * @param array{max_open?: int, max_idle?: int, max_lifetime?: float, max_idle_time?: float, acquire_timeout?: float} $options
         */
        public static function open(string $dsn, array $options = []): self
        {
            $maxOpen = (int) ($options['max_open'] ?? 10);
            $pool = self::call('sql.open', [
                'dsn' => $dsn,
                'max_open' => $maxOpen,
                'max_idle' => (int) ($options['max_idle'] ?? ($maxOpen > 0 ? $maxOpen : 10)),
                'max_lifetime' => (float) ($options['max_lifetime'] ?? 0),
                'max_idle_time' => (float) ($options['max_idle_time'] ?? 0),
                'acquire_timeout' => (float) ($options['acquire_timeout'] ?? 0),
            ]);
            return new self((int) $pool);
        }

        /**
         * Run $fn in a transaction: committed when $fn returns, rolled back
         * when it throws (the exception is rethrown). Returns what $fn returns.
         * A transaction left open when its fiber ends is rolled back.
         *
         * @template T
         * @param callable(Tx): T $fn
         * @param array{isolation?: 'read committed'|'repeatable read'|'serializable', read_only?: bool} $options
         * @return T
         */
        public function transaction(callable $fn, array $options = []): mixed
        {
            $tx = new Tx((int) self::call('sql.begin', [
                'pool' => $this->pool,
                'isolation' => (string) ($options['isolation'] ?? ''),
                'read_only' => (bool) ($options['read_only'] ?? false),
            ]));
            try {
                $result = $fn($tx);
            } catch (\Throwable $e) {
                $tx->end(false);
                throw $e;
            }
            $tx->end(true);
            return $result;
        }

        /** @return array{max_open: int, open: int, in_use: int, idle: int, wait_count: int, wait_time: float} */
        public function stats(): array
        {
            return self::call('sql.stats', ['pool' => $this->pool]);
        }

        protected function target(): array
        {
            return ['pool' => $this->pool];
        }

        /** @internal */
        public static function call(string $op, array $payload): mixed
        {
            $res = Go::call($op, $payload);
            if (is_array($res) && isset($res['pool_timeout'])) {
                throw new \Cairo\PoolTimeout((string) $res['error']);
            }
            if (is_array($res) && isset($res['error'])) {
                throw new SqlException((string) $res['error'], $res['sqlstate'] ?? null, $res['detail'] ?? null);
            }
            return $res;
        }
    }

    /** A transaction; see Db::transaction(). */
    final class Tx extends Queryable
    {
        private bool $ended = false;

        /** @internal */
        public function __construct(private readonly int $tx)
        {
        }

        protected function target(): array
        {
            if ($this->ended) {
                throw new SqlException('the transaction has ended');
            }
            return ['tx' => $this->tx];
        }

        /** @internal commit (true) or roll back */
        public function end(bool $commit): void
        {
            if ($this->ended) {
                return;
            }
            $this->ended = true;
            try {
                Db::call('sql.end', ['tx' => $this->tx, 'commit' => $commit]);
            } catch (SqlException $e) {
                if ($commit) {
                    throw $e;
                }
            }
        }
    }
}

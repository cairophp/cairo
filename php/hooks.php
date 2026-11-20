<?php
/**
 * Runtime hooks: the Cairo\Hook flags that turn them on, and what Cairo's
 * C hooks call when a hooked blocking function runs inside a fiber.
 */

declare(strict_types=1);

namespace Cairo {

    /**
     * Runtime hooks: blocking PHP functions that suspend the current fiber
     * instead of blocking the whole thread. See App::hooks().
     */
    final class Hook
    {
        public const NONE = 0;
        /** sleep(), usleep(), time_nanosleep() */
        public const SLEEP = 1;
        /** reads and writes on tcp:// and unix:// streams: mysqlnd (PDO MySQL, mysqli), Redis clients, fsockopen() */
        public const SOCKETS = 2;
        /** curl_exec(): curl's socket waits suspend the fiber */
        public const CURL = 16;
        /** connects on tcp:// and unix:// streams, through Go's dialer (DNS included) */
        public const CONNECT = 4;
        /** gethostbyname(), gethostbynamel(), through Go's resolver */
        public const DNS = 8;
        /** PDO pgsql: Cairo's Go-backed driver runs each query on the fiber's goroutine */
        public const PGSQL = 32;
        public const ALL = self::SLEEP | self::SOCKETS | self::CONNECT | self::DNS | self::CURL | self::PGSQL;
    }
}

namespace Cairo\Internal {

    use Cairo\Context;

    /**
     * Called from Cairo's C hooks when a hooked blocking function runs inside
     * a fiber. Returns false to let the original function block as usual.
     */
    final class Hooks
    {
        public static function sleep(float $seconds): bool
        {
            if (!self::on(\Cairo\Hook::SLEEP)) {
                return false;
            }
            Scheduler::instance()->waits->sleep($seconds);
            return true;
        }

        /**
         * Wait until socket $fd is readable (or writable). $timeout is the
         * stream's own timeout in seconds, -1 for none. Returns 1 when ready,
         * -1 on the stream timeout, 0 to let PHP block as usual.
         */
        public static function wait(int $fd, bool $write, float $timeout): int
        {
            if (!self::on(\Cairo\Hook::SOCKETS)) {
                return 0;
            }
            $scheduler = Scheduler::instance();
            if ($scheduler->waitHere($fd, $write)) {
                return 1;
            }
            $result = $scheduler->submitted(\cairo_fiber_wait(Context::current()->fiberId, $fd, $write, $timeout));
            return $result === 'timeout' ? -1 : 1;
        }

        /**
         * Wait for any of curl's sockets ($read readable, $write writable) or
         * $timeout seconds (-1: none). Returns 1 when done waiting, 0 to let
         * curl block as usual.
         *
         * @param list<int> $read
         * @param list<int> $write
         */
        public static function waitSet(array $read, array $write, float $timeout): int
        {
            if (!self::on(\Cairo\Hook::CURL)) {
                return 0;
            }
            Scheduler::instance()->submitted(\cairo_fiber_waitset(Context::current()->fiberId, $read, $write, $timeout));
            return 1;
        }

        /**
         * Connect $address ("host:port", or a path for unix) through Go.
         * Returns the connected socket's descriptor, an error message, or
         * null to let PHP connect itself.
         */
        /** @param array<string, mixed> $tls the "ssl" context options, for $network "tls" */
        public static function connect(string $network, string $address, float $timeout, array $tls = []): int|string|null
        {
            if (!self::on(\Cairo\Hook::CONNECT)) {
                return null;
            }
            $dial = ['network' => $network, 'address' => $address, 'timeout' => $timeout];
            if ($network === 'tls') {
                $dial['tls'] = $tls;
            }
            $result = Scheduler::instance()->call('dial', json_encode($dial, JSON_THROW_ON_ERROR));
            return isset($result['fd']) ? (int) $result['fd'] : (string) ($result['err'] ?? 'connect failed');
        }

        /**
         * stream_socket_enable_crypto() on socket $fd: Go handshakes as a TLS
         * client for $host. Returns the descriptor to put in place of $fd, an
         * error message, or null to let the thread wait for Go instead.
         *
         * @param array<string, mixed> $tls the "ssl" context options
         */
        public static function startTLS(int $fd, string $host, float $timeout, array $tls): int|string|null
        {
            if (!self::on(\Cairo\Hook::CONNECT)) {
                return null;
            }
            $request = ['fd' => $fd, 'host' => $host, 'timeout' => $timeout, 'tls' => $tls];
            $result = Scheduler::instance()->call('starttls', json_encode($request, JSON_THROW_ON_ERROR));
            return isset($result['fd']) ? (int) $result['fd'] : (string) ($result['err'] ?? 'TLS handshake failed');
        }

        public static function stopTLS(int $fd, float $timeout): int|string|null
        {
            if (!self::on(\Cairo\Hook::CONNECT)) {
                return null;
            }
            $request = ['fd' => $fd, 'timeout' => $timeout];
            $result = Scheduler::instance()->call('stoptls', json_encode($request, JSON_THROW_ON_ERROR));
            return isset($result['fd']) ? (int) $result['fd'] : (string) ($result['err'] ?? 'TLS shutdown failed');
        }

        public static function gethostbyname(mixed $hostname = null): ?string
        {
            if (!is_string($hostname) || !self::on(\Cairo\Hook::DNS)) {
                return null;
            }
            $ips = self::lookup($hostname);
            return $ips[0] ?? $hostname;   // like PHP: the name itself on failure
        }

        public static function gethostbynamel(mixed $hostname = null): array|false|null
        {
            if (!is_string($hostname) || !self::on(\Cairo\Hook::DNS)) {
                return null;
            }
            $ips = self::lookup($hostname);
            return $ips === [] ? false : $ips;
        }

        /**
         * One request of Cairo's PDO pgsql driver (JSON in, JSON out), run on
         * the fiber's goroutine. Null lets the driver wait for Go instead.
         */
        public static function pg(string $request): ?string
        {
            if (!self::on(\Cairo\Hook::PGSQL)) {
                return null;
            }
            $result = Scheduler::instance()->call('pg', $request);
            return json_encode($result, JSON_THROW_ON_ERROR);
        }

        /** is_uploaded_file(): true for this thread's Cairo uploads. */
        public static function isUploadedFile(mixed $path = null): ?bool
        {
            return is_string($path) && Form::isUpload($path) ? true : null;
        }

        /** move_uploaded_file() for Cairo uploads; null leaves others to PHP. */
        public static function moveUploadedFile(mixed $from = null, mixed $to = null): ?bool
        {
            if (!is_string($from) || !is_string($to) || !Form::isUpload($from)) {
                return null;
            }
            return Form::move($from, $to);
        }

        /** @return list<string> */
        private static function lookup(string $host): array
        {
            if (ip2long($host) !== false) {
                return [$host];
            }
            return (array) Scheduler::instance()->call('lookup', json_encode(['host' => $host], JSON_THROW_ON_ERROR));
        }

        /** Hooks run only in fibers Cairo started on a request thread. */
        private static function on(int $hook): bool
        {
            return Scheduler::running()
                && (Scheduler::instance()->app()->hookFlags() & $hook) !== 0
                && Context::current()->fiberId !== 0;
        }
    }
}

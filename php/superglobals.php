<?php
/**
 * PHP's superglobals for an HTTP request: isolated mode fills them, and
 * the framework adapters build their requests from the same values.
 */

declare(strict_types=1);

namespace Cairo\Internal {

    /** Fills PHP's superglobals for isolated-mode requests. */
    final class Superglobals
    {
        /** @var ?array<string, mixed> what the app put in $_SERVER as its thread started */
        private static ?array $thread = null;

        public static function fill(array $event): void
        {
            // Symfony's runtime and Dotenv put the app's settings in $_SERVER
            // as the thread starts, and read them there. The process's own
            // environment is left out: getenv() has it, and copying it into
            // every request measured 2.6% of a real request.
            self::$thread ??= array_filter(
                $_SERVER,
                static fn (mixed $value, int|string $key): bool => is_string($key) && !str_starts_with($key, 'HTTP_') && getenv($key) !== $value,
                ARRAY_FILTER_USE_BOTH,
            );
            $g = self::build($event);
            $_GET = $g['get'];
            $_POST = $g['post'];
            $_COOKIE = $g['cookies'];
            $_SERVER = $g['server'] + self::$thread;
            $_REQUEST = array_merge($g['get'], $g['post']);
            $_FILES = $g['files'];
        }

        /**
         * What the superglobals would hold for this request.
         * @return array{get: array, post: array, cookies: array, server: array, files: array}
         */
        public static function build(array $event): array
        {
            $headers = $event['headers'] ?? [];
            parse_str((string) ($event['query'] ?? ''), $get);
            $cookies = [];
            $server = [
                'REQUEST_METHOD' => $event['method'],
                'REQUEST_URI' => $event['path'] . (($event['query'] ?? '') !== '' ? '?' . $event['query'] : ''),
                'SCRIPT_NAME' => '',
                'PATH_INFO' => $event['path'],
                'QUERY_STRING' => (string) ($event['query'] ?? ''),
                'SERVER_PROTOCOL' => (string) ($event['proto'] ?? 'HTTP/1.1'),
                'HTTPS' => !empty($event['tls']) ? 'on' : '',
                'REQUEST_TIME_FLOAT' => microtime(true),
                'REQUEST_TIME' => time(),
            ];
            [$host, $port] = self::split((string) ($event['remote'] ?? ''));
            $server['REMOTE_ADDR'] = $host;
            $server['REMOTE_PORT'] = $port;
            $server['SERVER_NAME'] = explode(':', (string) ($event['host'] ?? ''))[0];
            // Go keeps the Host header apart from the others.
            if (($event['host'] ?? '') !== '') {
                $server['HTTP_HOST'] = (string) $event['host'];
            }
            foreach ($headers as $name => $values) {
                $value = implode(', ', (array) $values);
                $key = strtoupper(str_replace('-', '_', (string) $name));
                if ($key === 'CONTENT_TYPE' || $key === 'CONTENT_LENGTH') {
                    $server[$key] = $value;
                } else {
                    $server['HTTP_' . $key] = $value;
                }
                if ($key === 'COOKIE') {
                    foreach (explode(';', $value) as $pair) {
                        [$k, $v] = array_pad(explode('=', trim($pair), 2), 2, '');
                        if ($k !== '') {
                            $cookies[urldecode($k)] = urldecode($v);
                        }
                    }
                }
            }
            return [
                'get' => $get,
                'post' => Form::fields($event),
                'cookies' => $cookies,
                'server' => $server,
                'files' => Form::superglobal($event['files'] ?? []),
            ];
        }

        /** @return array{string, string} */
        private static function split(string $addr): array
        {
            $i = strrpos($addr, ':');
            if ($i === false) {
                return [$addr, ''];
            }
            return [trim(substr($addr, 0, $i), '[]'), substr($addr, $i + 1)];
        }
    }
}

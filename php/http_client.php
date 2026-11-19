<?php
/**
 * Cairo\Http\Client: an HTTP client whose requests wait in Go.
 */

declare(strict_types=1);

namespace Cairo\Http {

    /**
     * An HTTP client backed by Go's net/http: pooled keep-alive connections,
     * HTTP/2 over TLS, and proxies from HTTP_PROXY/HTTPS_PROXY/NO_PROXY.
     * Inside a request handler or async() task the fiber suspends while Go
     * sends the request, and the request's deadline cancels it; elsewhere
     * the thread waits.
     *
     *     $github = new Client(['base_uri' => 'https://api.github.com', 'timeout' => 5]);
     *     $repo = $github->get('/repos/php/php-src')->json();
     *
     * Options (to the constructor, or per request, which wins):
     *   base_uri         prefix for relative URLs
     *   headers          array<string, string|list<string>>
     *   query            array, appended to the URL
     *   body             string; or json (any value) or form (array)
     *   timeout          seconds for the whole exchange, 0 for none (default 30)
     *   follow_redirects bool, or the most redirects to follow (default 5)
     *   verify           bool, or a CA bundle path (default true)
     *   cert             client certificate PEM path, or [cert, key]
     *
     * Responses of any status are returned; transport failures throw
     * ClientException (ClientTimeoutException for the timeout option).
     */
    final class Client
    {
        /** @param array<string, mixed> $options */
        public function __construct(private readonly array $options = [])
        {
        }

        /** @param array<string, mixed> $options */
        public function get(string $url, array $options = []): ClientResponse
        {
            return $this->request('GET', $url, $options);
        }

        /** @param array<string, mixed> $options */
        public function post(string $url, array $options = []): ClientResponse
        {
            return $this->request('POST', $url, $options);
        }

        /** @param array<string, mixed> $options */
        public function put(string $url, array $options = []): ClientResponse
        {
            return $this->request('PUT', $url, $options);
        }

        /** @param array<string, mixed> $options */
        public function patch(string $url, array $options = []): ClientResponse
        {
            return $this->request('PATCH', $url, $options);
        }

        /** @param array<string, mixed> $options */
        public function delete(string $url, array $options = []): ClientResponse
        {
            return $this->request('DELETE', $url, $options);
        }

        /** @param array<string, mixed> $options */
        public function head(string $url, array $options = []): ClientResponse
        {
            return $this->request('HEAD', $url, $options);
        }

        /** @param array<string, mixed> $options */
        public function request(string $method, string $url, array $options = []): ClientResponse
        {
            $o = $options + $this->options;
            $headers = [];
            foreach ([$this->options['headers'] ?? [], $options['headers'] ?? []] as $set) {
                foreach ($set as $name => $value) {
                    $headers[strtolower((string) $name)] = [(string) $name, array_map('strval', (array) $value)];
                }
            }
            $setDefault = function (string $name, string $value) use (&$headers): void {
                $headers[strtolower($name)] ??= [$name, [$value]];
            };

            $body = '';
            if (array_key_exists('json', $o)) {
                $body = json_encode($o['json'], JSON_THROW_ON_ERROR | JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE);
                $setDefault('Content-Type', 'application/json');
            } elseif (isset($o['form'])) {
                $body = http_build_query($o['form'], '', '&', PHP_QUERY_RFC1738);
                $setDefault('Content-Type', 'application/x-www-form-urlencoded');
            } elseif (isset($o['body'])) {
                $body = (string) $o['body'];
            }

            if (isset($o['base_uri']) && !preg_match('~^[a-z][a-z0-9+.-]*://~i', $url)) {
                $url = rtrim((string) $o['base_uri'], '/') . '/' . ltrim($url, '/');
            }
            if (!empty($o['query'])) {
                $url .= (str_contains($url, '?') ? '&' : '?') . http_build_query($o['query'], '', '&', PHP_QUERY_RFC3986);
            }

            $follow = $o['follow_redirects'] ?? 5;
            $verify = $o['verify'] ?? true;
            $cert = (array) ($o['cert'] ?? []);
            $wire = [];
            foreach ($headers as [$name, $values]) {
                $wire[$name] = $values;
            }
            $result = \Cairo\Internal\Go::call('http', [
                'method' => strtoupper($method),
                'url' => $url,
                'headers' => (object) $wire,
                'body' => base64_encode($body),
                'timeout' => (float) ($o['timeout'] ?? 30),
                'redirects' => $follow === true ? 5 : (int) $follow,
                'tls' => [
                    'verify' => $verify !== false,
                    'cafile' => is_string($verify) ? $verify : '',
                    'cert' => (string) ($cert[0] ?? ''),
                    'key' => (string) ($cert[1] ?? ''),
                ],
            ]);
            if (!($result['ok'] ?? false)) {
                $message = (string) ($result['error'] ?? 'request failed');
                $kind = (string) ($result['kind'] ?? 'connect');
                throw $kind === 'timeout'
                    ? new ClientTimeoutException($message, $kind)
                    : new ClientException($message, $kind);
            }
            return new ClientResponse(
                (int) $result['status'],
                (string) $result['reason'],
                $result['headers'] ?: [],
                base64_decode((string) $result['body']),
                (string) $result['url'],
                (string) $result['protocol'],
            );
        }
    }

    final class ClientResponse
    {
        /** @param array<string, list<string>> $headers canonical names (Content-Type) */
        public function __construct(
            public readonly int $status,
            public readonly string $reason,
            public readonly array $headers,
            public readonly string $body,
            public readonly string $url,
            public readonly string $protocol,
        ) {
        }

        /** The first value of a header, by any case of its name. */
        public function header(string $name): ?string
        {
            foreach ($this->headers as $key => $values) {
                if (strcasecmp($key, $name) === 0) {
                    return $values[0] ?? null;
                }
            }
            return null;
        }

        public function ok(): bool
        {
            return $this->status >= 200 && $this->status < 300;
        }

        public function json(bool $assoc = true): mixed
        {
            return json_decode($this->body, $assoc, 512, JSON_THROW_ON_ERROR);
        }
    }

    /** A request that got no response: DNS, connect, TLS, redirects. */
    class ClientException extends \RuntimeException
    {
        /** $kind: connect, tls, timeout, redirects or request */
        public function __construct(string $message, public readonly string $kind = 'connect')
        {
            parent::__construct($message);
        }
    }

    /** The request's timeout option ran out. */
    final class ClientTimeoutException extends ClientException
    {
    }
}

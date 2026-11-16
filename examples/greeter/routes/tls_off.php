<?php

// Turning TLS off on a stream: stream_socket_enable_crypto($s, false), as an
// FTP client does after CCC. Go sends the close_notify, waits for the
// server's, and the stream carries on in plain text (and can turn TLS on
// again).

declare(strict_types=1);

use Cairo\Http\Request;
use Cairo\Http\Response;

if (!function_exists('cairo_example_tls_off')) {
    /** @return array<string, mixed> */
    function cairo_example_tls_off(string $addr, string $cafile): array
    {
        $ctx = stream_context_create(['ssl' => ['cafile' => $cafile]]);
        $s = stream_socket_client("tcp://$addr", $errno, $errstr, 5, STREAM_CLIENT_CONNECT, $ctx);
        if ($s === false) {
            return ['error' => $errstr];
        }
        $out = ['greeting' => trim((string) fgets($s))];
        $starttls = function () use ($s): bool {
            fwrite($s, "STARTTLS\r\n");
            fgets($s);
            return stream_socket_enable_crypto($s, true, STREAM_CRYPTO_METHOD_TLS_CLIENT) === true;
        };
        $out['on'] = $starttls();
        fwrite($s, "ECHO one\r\n");
        $out['secure'] = trim((string) fgets($s));
        fwrite($s, "CCC\r\n");
        $out['ccc'] = trim((string) fgets($s));
        $off = @stream_socket_enable_crypto($s, false);
        $out['off'] = $off;
        if ($off !== true) {
            $out['error'] = error_get_last()['message'] ?? '';
            return $out;
        }
        // Sent over TLS just before the server's close_notify, read after.
        $out['last_secure'] = trim((string) fgets($s));
        fwrite($s, "PLAIN two\r\n");
        $out['plain'] = trim((string) fgets($s));
        $out['on_again'] = $starttls();
        fwrite($s, "ECHO three\r\n");
        $out['secure_again'] = trim((string) fgets($s));
        $out['not_on'] = null;
        fclose($s);

        // Turning TLS off on a plain stream fails with a warning.
        $plain = stream_socket_client("tcp://$addr", $errno, $errstr, 5);
        fgets($plain);
        $out['not_on'] = @stream_socket_enable_crypto($plain, false);
        $out['not_on_error'] = error_get_last()['message'] ?? '';
        fclose($plain);
        return $out;
    }
}

return function ($http): void {
    $http->get('/tls-off', function (Request $req): Response {
        $args = [(string) $req->query('addr'), (string) $req->query('cafile')];
        if ($req->query('offload') !== null) {
            return Response::json(Cairo\offload('cairo_example_tls_off', ...$args));
        }
        return Response::json(cairo_example_tls_off(...$args));
    });
};

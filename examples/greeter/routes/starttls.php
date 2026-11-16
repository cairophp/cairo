<?php

// STARTTLS: stream_socket_enable_crypto() on a plain TCP stream, as SMTP,
// IMAP and mysqlnd do. Go performs the handshake.

declare(strict_types=1);

use Cairo\Http\Request;
use Cairo\Http\Response;

if (!function_exists('cairo_example_starttls')) {
    /**
     * Talks to a line protocol: greeting, "STARTTLS", then "ECHO <x>" over TLS.
     *
     * @return array<string, mixed>
     */
    function cairo_example_starttls(string $addr, string $cafile, int $method = STREAM_CRYPTO_METHOD_TLS_CLIENT): array
    {
        $ctx = stream_context_create(['ssl' => $cafile !== '' ? ['cafile' => $cafile] : []]);
        $s = stream_socket_client("tcp://$addr", $errno, $errstr, 5, STREAM_CLIENT_CONNECT, $ctx);
        if ($s === false) {
            return ['error' => $errstr];
        }
        $greeting = trim((string) fgets($s));
        fwrite($s, "STARTTLS\r\n");
        $go = trim((string) fgets($s));
        $enabled = @stream_socket_enable_crypto($s, true, $method);
        $out = ['greeting' => $greeting, 'go' => $go, 'enabled' => $enabled];
        if ($enabled !== true) {
            $out['error'] = error_get_last()['message'] ?? '';
            fclose($s);
            return $out;
        }
        fwrite($s, "ECHO hello\r\n");
        $out['echo'] = trim((string) fgets($s));
        fclose($s);
        return $out;
    }
}

return function ($http): void {
    $http->get('/starttls', function (Request $req): Response {
        $method = match ($req->query('method')) {
            'tls12' => STREAM_CRYPTO_METHOD_TLSv1_2_CLIENT,
            'server' => STREAM_CRYPTO_METHOD_TLS_SERVER,
            default => STREAM_CRYPTO_METHOD_TLS_CLIENT,
        };
        $addr = (string) $req->query('addr');
        $cafile = (string) $req->query('cafile');
        if ($req->query('offload') !== null) {
            return Response::json(Cairo\offload('cairo_example_starttls', $addr, $cafile, $method));
        }
        return Response::json(cairo_example_starttls($addr, $cafile, $method));
    });
};

<?php

// Cairo\Http\Client: Go's net/http from PHP.

declare(strict_types=1);

use Cairo\Http\Client;
use Cairo\Http\ClientException;
use Cairo\Http\Request;
use Cairo\Http\Response;

if (!function_exists('cairo_example_fetch')) {
    /** Runs on an offload thread, outside any fiber: the thread waits for Go. */
    function cairo_example_fetch(string $url): array
    {
        $res = (new Client())->get($url);
        return ['status' => $res->status, 'body' => $res->body];
    }
}

return function ($http): void {
    // GET ?url= and report what came back; the endpoint under test echoes.
    $http->get('/client/get', function (Request $req): Response {
        $res = (new Client(['timeout' => 10]))->get((string) $req->query('url'));
        return Response::json(['status' => $res->status, 'body' => $res->body, 'protocol' => $res->protocol]);
    });

    // coalesce: identical GETs in flight at once share one upstream call.
    // ?auth= sets the Authorization header, which is part of what must match.
    $http->get('/client/coalesce', function (Request $req): Response {
        $client = new Client(['timeout' => 10, 'coalesce' => true]);
        $res = $client->get((string) $req->query('url'), ['headers' => ['Authorization' => (string) $req->query('auth')]]);
        return Response::text($res->body);
    });

    // Exercises the options against an echo server at ?base=.
    $http->get('/client/features', function (Request $req): Response {
        $client = new Client([
            'base_uri' => (string) $req->query('base'),
            'headers' => ['X-Default' => 'd', 'X-Override' => 'old'],
            'timeout' => 5,
        ]);
        $out = [];

        $res = $client->post('/echo', ['json' => ['a' => 1], 'query' => ['q' => 'x y'], 'headers' => ['X-Override' => 'new', 'X-Multi' => ['1', '2']]]);
        $out['json'] = ['status' => $res->status, 'reason' => $res->reason, 'echo' => $res->json(), 'type' => $res->header('content-type'), 'ok' => $res->ok()];

        $res = $client->put('echo', ['form' => ['name' => 'Cairo', 'tags' => ['a', 'b']]]);
        $out['form'] = $res->json();

        $res = $client->post('/echo', ['body' => "\x00\xffraw", 'headers' => ['Content-Type' => 'application/octet-stream']]);
        $out['raw'] = bin2hex(base64_decode($res->json()['body64']));

        $res = $client->get('/redirect');
        $out['redirect'] = ['status' => $res->status, 'url' => $res->url];
        $res = $client->get('/redirect', ['follow_redirects' => false]);
        $out['no_redirect'] = ['status' => $res->status, 'location' => $res->header('Location')];

        $out['not_found'] = $client->delete('/missing')->status;
        $out['head'] = $client->head('/echo')->body;

        try {
            $client->get('/slow', ['timeout' => 0.2]);
            $out['timeout'] = 'no exception';
        } catch (ClientException $e) {
            $out['timeout'] = [get_class($e), $e->kind];
        }
        try {
            (new Client())->get('http://127.0.0.1:1/');
            $out['refused'] = 'no exception';
        } catch (ClientException $e) {
            $out['refused'] = $e->kind;
        }
        $tls = (string) $req->query('tls');
        try {
            (new Client())->get($tls);
            $out['tls_unverified'] = 'no exception';
        } catch (ClientException $e) {
            $out['tls_unverified'] = $e->kind;
        }
        $out['tls_cafile'] = (new Client(['verify' => (string) $req->query('cafile')]))->get($tls)->body;
        $out['tls_insecure'] = (new Client(['verify' => false]))->get($tls)->body;

        $out['offload'] = Cairo\offload('cairo_example_fetch', rtrim((string) $req->query('base'), '/') . '/echo');
        return Response::json($out);
    });
};

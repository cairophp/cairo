<?php

// Cairo\Net: connections, listeners, datagrams and DNS from Go's net package.

declare(strict_types=1);

use Cairo\Http\Request;
use Cairo\Http\Response;
use Cairo\Net;
use Cairo\Net\NetException;
use Cairo\Net\Resolver;
use Cairo\Net\TimeoutException;

if (!function_exists('cairo_example_net_line')) {
    /** Dial $addr and read one line; on an offload thread the thread waits for Go. */
    function cairo_example_net_line(string $addr): ?string
    {
        $conn = Net\dial('tcp', $addr, ['timeout' => 5]);
        $line = $conn->readLine();
        $conn->close();
        return $line;
    }
}

return function ($http): void {
    // A line echo server and its client, both in this request.
    $http->get('/net/echo', function (Request $req): Response {
        $listener = Net\listen('tcp', '127.0.0.1:0');
        $server = Cairo\async(function () use ($listener): int {
            $conn = $listener->accept(5);
            $lines = 0;
            while (($line = $conn->readLine()) !== null) {
                $conn->write('echo: ' . $line);
                $lines++;
            }
            $conn->close();
            return $lines;
        });

        $conn = Net\dial('tcp', $listener->address);
        $conn->write("one\ntwo\n");
        $out = ['first' => $conn->readLine(), 'second' => $conn->readExactly(10)];
        $conn->write("three\n");
        $out['third'] = $conn->read();
        $conn->closeWrite();
        $out['after_close'] = $conn->readLine();
        $out['lines'] = Cairo\await($server);
        $out['remote_is_listener'] = $conn->remoteAddress === $listener->address;

        $silent = Net\dial('tcp', $listener->address);   // accepted by the backlog, never answered
        try {
            $silent->setTimeout(0.2)->read();
            $out['timeout'] = 'no exception';
        } catch (TimeoutException $e) {
            $out['timeout'] = $e->kind;
        }
        $idle = Net\listen('tcp', '127.0.0.1:0');
        try {
            $idle->accept(0.2);
            $out['accept_timeout'] = 'no exception';
        } catch (TimeoutException $e) {
            $out['accept_timeout'] = $e->kind;
        }
        $listener->close();
        try {
            Net\dial('tcp', '127.0.0.1:1');
            $out['refused'] = 'no exception';
        } catch (NetException $e) {
            $out['refused'] = $e->kind;
        }
        return Response::json($out);
    });

    // TLS client against ?addr= (a server whose CA is ?cafile=), datagrams,
    // DNS through ?dns=, and the API outside a fiber.
    $http->get('/net/more', function (Request $req): Response {
        $out = [];
        $tls = Net\dial('tls', (string) $req->query('addr'), ['cafile' => (string) $req->query('cafile')]);
        $tls->write("ping\n");
        $out['tls'] = $tls->readLine();
        try {
            Net\dial('tls', (string) $req->query('addr'));
            $out['tls_unverified'] = 'no exception';
        } catch (NetException $e) {
            $out['tls_unverified'] = $e->kind;
        }

        $a = Net\listenPacket('udp', '127.0.0.1:0');
        $b = Net\listenPacket('udp', '127.0.0.1:0');
        $a->writeTo('datagram', $b->address);
        [$data, $from] = $b->readFrom(1024, 2);
        $out['udp'] = ['data' => $data, 'from_a' => $from === $a->address];

        $dns = new Resolver((string) $req->query('dns'), 2);
        $out['dns'] = [
            'host' => $dns->lookupIPv4('cairo.test'),
            'txt' => $dns->lookupTXT('cairo.test'),
            'mx' => $dns->lookupMX('cairo.test'),
        ];
        try {
            $dns->lookupHost('missing.test');
            $out['dns']['missing'] = 'no exception';
        } catch (NetException $e) {
            $out['dns']['missing'] = $e->kind;
        }
        $out['localhost'] = in_array('127.0.0.1', (new Resolver())->lookupHost('localhost'), true);
        $out['offload'] = Cairo\offload('cairo_example_net_line', (string) $req->query('plain'));
        return Response::json($out);
    });

    // Reads a line that the server at ?addr= sends after a pause.
    $http->get('/net/line', function (Request $req): Response {
        return Response::json(['line' => cairo_example_net_line((string) $req->query('addr'))]);
    });
};

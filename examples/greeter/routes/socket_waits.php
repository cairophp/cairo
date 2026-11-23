<?php

// Waits on sockets stay on the PHP thread, in its own epoll: a read ends at
// the stream's timeout or with its context, the thread still takes
// everything else while a fiber waits on a socket, and one fiber waits on a
// socket at a time. A read or a write is tried first and waits only when
// the socket is not ready. ?addr= is a server that reads a number of seconds,
// waits that long and answers "pong".

declare(strict_types=1);

use Cairo\Context;
use Cairo\Http\Request;
use Cairo\Http\Response;

use function Cairo\async;
use function Cairo\await;
use function Cairo\Net\dial;
use function Cairo\sleep;

return function ($http): void {
    /** @return resource a connection to ?addr= that answers in $delay seconds */
    $ask = static function (Request $req, float $delay) {
        $conn = stream_socket_client('tcp://' . $req->query('addr'), $errno, $error, 5);
        if ($conn === false) {
            throw new RuntimeException("connect: $error");
        }
        fwrite($conn, "$delay\n");
        return $conn;
    };

    // The stream's own timeout ends the read after 0.15s; the answer comes
    // at 0.4s, and the next read of the same socket gets it.
    $http->get('/sockets/timeout', function (Request $req) use ($ask): Response {
        $conn = $ask($req, 0.4);
        stream_set_timeout($conn, 0, 150000);
        $start = microtime(true);
        $first = fgets($conn);
        $took = microtime(true) - $start;
        $timedOut = stream_get_meta_data($conn)['timed_out'];
        stream_set_timeout($conn, 5);
        return Response::json([
            'first' => $first,
            'timed_out' => $timedOut,
            'took' => round($took, 2),
            'second' => trim((string) fgets($conn)),
        ]);
    });

    // A read ends with the context it runs under: at its deadline, or when
    // another fiber cancels it. Under one that has ended it does not wait.
    // Each case answers how the read ended and after how long, and for one
    // that its context ended, what the next read of the same socket got.
    $http->get('/sockets/context', function (Request $req, Context $ctx) use ($ask): Response {
        $read = static function (Context $c) use ($ask, $req): array {
            $conn = $ask($req, 0.4);
            $start = microtime(true);
            try {
                return [trim((string) $c->run(fn () => fgets($conn))), round(microtime(true) - $start, 2), null];
            } catch (Context\ContextException $e) {
                return [$e::class, round(microtime(true) - $start, 2), trim((string) fgets($conn))];
            }
        };
        $cancelSoon = static function (Context $c): Context {
            async(function () use ($c): void {
                sleep(0.1);
                $c->cancel();
            });
            return $c;
        };
        $deadline = $read($ctx->withTimeout(0.15));
        $canceled = $read($c = $cancelSoon($ctx->withCancel()));
        return Response::json([
            'deadline' => $deadline,
            'canceled' => $canceled,
            'ended' => $read($c),
            'live' => $read($ctx->withTimeout(30)),
        ]);
    });

    // A task waits a second for a socket. Next to it the request asks
    // ?addr= five times through Go (Cairo\Net) and sleeps 50 ms: the thread,
    // asleep in its epoll for the socket, wakes for each answer and for the
    // timer.
    $http->get('/sockets/beside', function (Request $req) use ($ask): Response {
        $conn = $ask($req, 1.0);
        $reader = async(fn (): string => trim((string) fgets($conn)));
        $slowest = 0.0;
        for ($i = 0; $i < 5; $i++) {
            $start = microtime(true);
            $go = dial('tcp', (string) $req->query('addr'), ['timeout' => 5]);
            $go->write("0\n");
            $go->readLine();
            $go->close();
            $slowest = max($slowest, microtime(true) - $start);
        }
        $start = microtime(true);
        sleep(0.05);
        $slept = microtime(true) - $start;
        return Response::json(['slowest_call' => round($slowest, 3), 'slept' => round($slept, 3), 'read' => await($reader)]);
    });

    // A write of 8 MiB to a server that starts reading after 0.3s: the
    // socket's buffer fills, the fiber waits for room and the thread runs a
    // sleep of another fiber meanwhile. ?addr= answers how many bytes it got.
    $http->get('/sockets/write', function (Request $req): Response {
        $conn = stream_socket_client('tcp://' . $req->query('addr'), $errno, $error, 5);
        if ($conn === false) {
            throw new RuntimeException("connect: $error");
        }
        $beside = async(function (): float {
            $start = microtime(true);
            sleep(0.05);
            return microtime(true) - $start;
        });
        $written = fwrite($conn, str_repeat('x', 8 << 20));
        stream_socket_shutdown($conn, STREAM_SHUT_WR);
        return Response::json(['written' => $written, 'got' => (int) fgets($conn), 'slept' => round(await($beside), 3)]);
    });

    // A server that says one line and closes: the read after it finds the
    // end of the stream, not a wait.
    $http->get('/sockets/end', function (Request $req): Response {
        $conn = stream_socket_client('tcp://' . $req->query('addr'), $errno, $error, 5);
        if ($conn === false) {
            throw new RuntimeException("connect: $error");
        }
        $line = fgets($conn);
        $start = microtime(true);
        $after = fgets($conn);
        return Response::json(['line' => $line, 'after' => $after, 'eof' => feof($conn), 'took' => round(microtime(true) - $start, 2)]);
    });

    // Fifty tasks each wait on a socket of their own. ?addr= says each line
    // back after 0.3s, so the sockets are ready at the same moment: every
    // fiber is woken for its own.
    $http->get('/sockets/many', function (Request $req): Response {
        $start = microtime(true);
        $tasks = [];
        for ($i = 0; $i < 50; $i++) {
            $conn = stream_socket_client('tcp://' . $req->query('addr'), $errno, $error, 5);
            if ($conn === false) {
                throw new RuntimeException("connect: $error");
            }
            fwrite($conn, "$i\n");
            $tasks[$i] = async(fn (): string => trim((string) fgets($conn)));
        }
        $own = 0;
        foreach ($tasks as $i => $task) {
            $own += await($task) === (string) $i ? 1 : 0;
        }
        return Response::json(['own' => $own, 'took' => round(microtime(true) - $start, 2)]);
    });

    // A second fiber that reads a socket another one waits on is refused.
    $http->get('/sockets/shared', function (Request $req) use ($ask): Response {
        $conn = $ask($req, 0.2);
        $first = async(fn (): string => trim((string) fgets($conn)));
        try {
            $second = fgets($conn);
        } catch (RuntimeException $e) {
            $second = $e->getMessage();
        }
        return Response::json(['first' => await($first), 'second' => $second]);
    });
};

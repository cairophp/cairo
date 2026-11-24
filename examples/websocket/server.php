<?php

// WebSockets and topics. A WebSocket is served by a fiber of PHP: fifty
// clients that each wait a second for their message share one PHP thread.
// A topic is published to once, and Go sends the message to every
// WebSocket that subscribed and to every Server-Sent Events client of
// Response::eventsFrom(), which holds no PHP at all.
//
//   cairo serve examples/websocket/server.php
//   then, in a browser's console on http://localhost:8080/ :
//     ws = new WebSocket('ws://localhost:8080/chat?name=amira'); ws.onmessage = e => console.log(e.data); ws.send('hello')

declare(strict_types=1);

use Cairo\App;
use Cairo\Context;
use Cairo\Http\Request;
use Cairo\Http\Response;
use Cairo\Http\Topic;
use Cairo\Http\WebSocket;
use Cairo\Middleware;

use function Cairo\sleep;

/** What the handlers saw of their clients, read back through /seen. */
final class Seen
{
    /** @var array<string, mixed> */
    public static array $log = [];
}

$app = new App();
if (($mode = getenv('CAIRO_CONCURRENCY')) !== false) {
    $app->concurrency($mode);   // 'isolated': a socket then holds its PHP thread, /news still holds none
}
$http = $app->http(getenv('CAIRO_HTTP') ?: ':8080');
if (($limit = getenv('CAIRO_WRITE_TIMEOUT')) !== false) {
    $http->timeouts(write: (float) $limit);
}
if (($limit = getenv('CAIRO_TIMEOUT')) !== false) {
    $http->use(Middleware::timeout((float) $limit));
}

$http->get('/', fn (): Response => Response::html('<p>Open the console: see examples/websocket/server.php.</p>'));

// Every message comes back as it was, text or binary.
$http->get('/echo', fn (Request $req): Response => Response::websocket(function (WebSocket $ws): void {
    while (($message = $ws->receive()) !== null) {
        $ws->send($message->data, $message->binary);
    }
    Seen::$log['echo'] = 'the client closed';
}, maxMessage: (int) ($req->query('max') ?? '65536')))->noTimeout();

// A chat room: every client hears what any of them says. The fiber only
// reads; Go sends the room's messages to its members. What is deferred
// runs when the socket has ended, however it ends.
$http->get('/chat', function (Request $req, Context $ctx): Response {
    $name = $req->query('name') ?? 'someone';
    return Response::websocket(function (WebSocket $ws) use ($name, $ctx): void {
        $ws->subscribe('room');
        $ctx->defer(static fn () => Topic::publish('room', "$name left"));
        Topic::publish('room', "$name joined");
        while (($message = $ws->receive()) !== null) {
            Topic::publish('room', "$name: {$message->data}");
        }
    });
})->noTimeout();

// The handler of the route decides first: only a Response::websocket()
// makes the handshake.
$http->get('/members', fn (Request $req): Response => $req->header('Authorization') === 'Bearer let-me-in'
    ? Response::websocket(static fn (WebSocket $ws) => $ws->send('welcome'))
    : Response::text("who are you?\n", 401));

// A page of another site may connect only when its origin is named.
$http->get('/partners', fn (): Response => Response::websocket(
    static fn (WebSocket $ws) => $ws->send('hello, partner'),
    origins: ['*.partner.example'],
));

// The client chooses among the subprotocols that the server offers.
$http->get('/protocols', fn (): Response => Response::websocket(
    static fn (WebSocket $ws) => $ws->send('speaking ' . $ws->protocol),
    protocols: ['chat.v2', 'chat.v1'],
));

// The server closes, with a status of the app's own.
$http->get('/bye', fn (): Response => Response::websocket(function (WebSocket $ws): void {
    $ws->send('last words');
    $ws->close(4001, 'that was all');
    Seen::$log['bye'] = $ws->receive() === null ? 'closed' : 'still open';
}));

// The server only sends. It learns that its client left when it sends.
$http->get('/ticks', fn (): Response => Response::websocket(function (WebSocket $ws): void {
    try {
        for ($i = 1; ; $i++) {
            $ws->send("tick $i");
            sleep(0.05);
        }
    } catch (Context\Canceled $e) {
        Seen::$log['ticks'] = 'the client left';
        throw $e;
    }
}, ping: (float) (getenv('CAIRO_PING') ?: 30)))->noTimeout();

// The message comes a second after the socket opens: the wait is the fiber's.
$http->get('/late', fn (): Response => Response::websocket(function (WebSocket $ws): void {
    sleep(1);
    $ws->send('late');
}));

// A handler that fails: the client is told that the server had an error.
$http->get('/fails', fn (): Response => Response::websocket(function (WebSocket $ws): void {
    $ws->send('so far so good');
    throw new RuntimeException('failed halfway');
}));

// What the client sent before it left is still received, and the handler
// is not cut off while it works on it: receive() says null after the last.
$http->get('/count', fn (): Response => Response::websocket(function (WebSocket $ws): void {
    sleep(0.3);   // the client sends three messages and closes meanwhile
    $got = [];
    while (($message = $ws->receive()) !== null) {
        $got[] = $message->data;
    }
    Seen::$log['count'] = implode(',', $got);
}));

// Server-Sent Events from topics: PHP decides who listens to what, and its
// fiber ends. Go serves the client from there.
$http->get('/news', fn (Request $req): Response => $req->query('token') === 'reader'
    ? Response::eventsFrom(['news', 'user.' . ($req->query('user') ?? 'nobody')], keepAlive: (float) ($req->query('keep') ?? '15'))
    : Response::text("no token\n", 403));

// Any code publishes: here a plain request. The answer is how many listen.
$http->post('/publish/{topic}', fn (Request $req): Response => Response::json([
    'listening' => Topic::publish((string) $req->param('topic'), $req->body(), $req->query('event'), $req->query('id')),
]));

// The PHP memory of this thread: a client of /news adds nothing to it.
$http->get('/memory', fn (): Response => Response::text((string) memory_get_usage()));

$http->get('/seen', fn (): Response => Response::json(Seen::$log));

return $app;

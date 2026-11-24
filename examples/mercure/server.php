<?php

// A Mercure hub in the server: browsers subscribe to topics at
// /.well-known/mercure, PHP publishes with mercure_publish(), and Go serves
// every subscriber, so no PHP thread is held for them.
//
//   MERCURE_KEY=$(openssl rand -hex 32) cairo serve examples/mercure/server.php
//   curl -N 'http://localhost:8080/.well-known/mercure?match=https://example.com/news'
//   curl -d 'the news' 'http://localhost:8080/publish'

declare(strict_types=1);

use Cairo\App;
use Cairo\Context;
use Cairo\Http\Request;
use Cairo\Http\Response;

use function Cairo\sleep;

$app = new App();
if (getenv('CAIRO_CONCURRENCY') === 'isolated') {
    $app->concurrency('isolated');
}
$http = $app->http(getenv('CAIRO_HTTP') ?: ':8080');
if (($limit = getenv('CAIRO_TIMEOUT')) !== false) {
    $http->use(Cairo\Middleware::timeout((float) $limit));
}

// Anyone may subscribe to what is public. The key, from the environment,
// signs the tokens of the programs that publish with a POST and of the
// clients that may hear private updates; the hub answers for one public URL.
$url = getenv('MERCURE_URL') ?: 'http://localhost:8080';
$hub = $http->mercure()->anonymous()->publicUrls([$url])->cookie('mercure_access_token');
if (($key = (string) getenv('MERCURE_KEY')) !== '') {
    $hub->issuer($url, publisher: $key, subscriber: $key);
}
if (($file = (string) getenv('MERCURE_HISTORY')) !== '') {
    $hub->history($file, size: 1000);
}
if (($version = (int) getenv('MERCURE_COMPATIBILITY')) !== 0) {
    $hub->compatibility($version);
}
if (getenv('MERCURE_SUBSCRIPTIONS') === '1') {
    $hub->subscriptions();
}

// The hub's own metrics (mercure_*) join Cairo's where a server has them.
if (($ops = (string) getenv('CAIRO_OPS')) !== '') {
    $app->http($ops)->metrics();
}

$topic = 'https://example.com/news';

// The body is the update. ?topic=, ?private=1, ?type= and ?id= say the rest.
$http->post('/publish', function (Request $req) use ($topic): Response {
    if (($wait = (float) $req->query('wait')) > 0) {
        sleep($wait);   // only this fiber waits
    }
    if ($req->query('after') === '1') {
        // The client has its answer before the subscribers have the update.
        Context::defer(static fn () => mercure_publish($topic, $req->body()));
        return Response::text('', 202);
    }
    try {
        $id = mercure_publish(
            $req->query('topic') ?? $topic,
            $req->body(),
            private: $req->query('private') === '1',
            id: $req->query('id'),
            type: $req->query('type'),
        );
    } catch (ValueError $e) {
        return Response::text($e->getMessage() . "\n", 422);
    }
    return Response::json(['id' => $id]);
});

$http->get('/', fn (): Response => Response::html(<<<HTML
    <!doctype html>
    <title>Mercure on Cairo</title>
    <ul id="news"></ul>
    <script>
      const source = new EventSource('/.well-known/mercure?match=' + encodeURIComponent('$topic'));
      source.onmessage = (e) => news.insertAdjacentHTML('beforeend', '<li></li>') || (news.lastChild.textContent = e.data);
    </script>
    HTML));

$http->get('/memory', fn (): Response => Response::json(['php' => memory_get_usage()]));

return $app;

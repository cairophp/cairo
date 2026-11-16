<?php

// Isolated mode: one request per PHP thread at a time, for code that keeps
// request state in globals or singletons. Requests get superglobals and may
// echo their output and call header(), like classic PHP.

declare(strict_types=1);

use Cairo\App;
use Cairo\Resettable;

/** A service with per-request state, reset after every request. */
final class CurrentUser implements Resettable
{
    public static ?self $instance = null;
    public ?string $name = null;
    public int $resets = 0;

    public static function get(): self
    {
        return self::$instance ??= new self();
    }

    public function reset(): void
    {
        $this->name = null;
        $this->resets++;
    }
}

$app = (new App())
    ->concurrency('isolated')
    ->resetAfterRequest(CurrentUser::get());

$http = $app->http(getenv('CAIRO_HTTP') ?: ':8080');

// Global state that would be clobbered if requests ran side by side: each
// request stores its own value, sleeps (other requests queue meanwhile), and
// reads it back.
$http->get('/legacy', function (): void {
    $GLOBALS['who'] = $_GET['who'] ?? '';
    CurrentUser::get()->name ??= $_GET['who'] ?? '';
    usleep(200_000);
    header('Content-Type: application/json');
    header('X-Legacy: yes');
    http_response_code(201);
    echo json_encode([
        'who' => $GLOBALS['who'],
        'user' => CurrentUser::get()->name,
        'method' => $_SERVER['REQUEST_METHOD'],
        'resets' => CurrentUser::get()->resets,
    ]);
});

$http->post('/form', function (): void {
    echo 'hello ' . ($_POST['name'] ?? '?') . ' (' . ($_COOKIE['session'] ?? 'no session') . ')';
});

return $app;

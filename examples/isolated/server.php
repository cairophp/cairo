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

// File uploads fill $_FILES; move_uploaded_file() and is_uploaded_file()
// work as in classic PHP.
$http->post('/upload', function (): void {
    $dir = sys_get_temp_dir() . '/cairo-isolated-uploads';
    @mkdir($dir);
    $moved = [];
    foreach ((array) ($_FILES['docs']['tmp_name'] ?? []) as $i => $tmp) {
        $target = $dir . '/' . basename($_FILES['docs']['name'][$i]);
        $moved[] = is_uploaded_file($tmp) && move_uploaded_file($tmp, $target) ? file_get_contents($target) : null;
        @unlink($target);
    }
    header('Content-Type: application/json');
    echo json_encode([
        'post' => $_POST,
        'avatar' => [
            'name' => $_FILES['avatar']['name'] ?? null,
            'size' => $_FILES['avatar']['size'] ?? null,
            'type' => $_FILES['avatar']['type'] ?? null,
            'error' => $_FILES['avatar']['error'] ?? null,
        ],
        'empty_error' => $_FILES['empty']['error'] ?? null,
        'docs' => $moved,
        'docs_names' => $_FILES['docs']['name'] ?? null,
        'fake_is_upload' => is_uploaded_file(__FILE__),
        'fake_move' => @move_uploaded_file(__FILE__, '/tmp/should-not-exist'),
    ]);
});

return $app;

<?php

// For TestDetectorsTogether: the repeated query detector and the leak
// detector on at once. /both keeps the caller's name in a static and, when
// asked, runs one statement three times.

declare(strict_types=1);

use Cairo\App;
use Cairo\Http\Request;
use Cairo\Http\Response;

final class Seen
{
    public static string $last = '';
}

$app = new App();
$app->repeatedQueries(3);
$app->detectLeaks();
$http = $app->http(getenv('CAIRO_HTTP') ?: ':8080');

$http->get('/both', function (Request $request): Response {
    Seen::$last = (string) $request->query('name');
    if ($request->query('queries') !== null) {
        $db = new PDO('sqlite::memory:', null, null, [PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION]);
        foreach (range(1, 3) as $id) {
            $db->prepare('SELECT ? AS id')->execute([$id]);
        }
    }
    return Response::text("ok\n");
});

return $app;

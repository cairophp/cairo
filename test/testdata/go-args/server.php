<?php

// For TestGoOperationArguments: what PHP values become on their way to a Go
// operation. /args passes each as the `tags` of cache.purge, which takes a
// list of strings, and answers what Go made of it: "ok", or why not.

declare(strict_types=1);

use Cairo\App;
use Cairo\Http\Response;
use Cairo\Internal\Go;

enum Tag: string
{
    case News = 'news';
}

final class Tags implements JsonSerializable
{
    /** @return list<string> */
    public function jsonSerialize(): array
    {
        return ['news', 'sport'];
    }
}

$app = new App();
$app->http(getenv('CAIRO_HTTP') ?: ':8080')->get('/args', function (): Response {
    $deep = [];
    for ($i = 0; $i < 70; $i++) {
        $deep = [$deep];
    }
    $self = [];
    $self[] = &$self;
    $memory = fopen('php://memory', 'r');
    $out = [];
    foreach ([
        'list' => ['news', 'sport'],
        'empty' => [],
        'null' => null,
        'not UTF-8' => ["\xff\x00"],
        'enum' => [Tag::News],
        'JsonSerializable' => new Tags(),
        'string keys' => ['a' => 'news'],
        'keys out of order' => [1 => 'news', 0 => 'sport'],
        'stdClass' => (object) ['a' => 'news'],
        'int' => 7,
        'float' => 1.5,
        'bool' => true,
        'string' => 'news',
        'resource' => $memory,
        'too deep' => $deep,
        'holds itself' => $self,
    ] as $name => $tags) {
        try {
            Go::callNow('cache.purge', ['tags' => $tags]);
            $out[$name] = 'ok';
        } catch (Throwable $e) {
            $out[$name] = $e::class . ': ' . $e->getMessage();
        }
    }
    fclose($memory);
    return Response::json($out);
});

return $app;

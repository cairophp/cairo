<?php

// Cairo\Cache: the PHP side of Go's response cache (see ->cache() on
// Cairo\Http\Server). Loaded by runtime.php's autoloader on first use.

declare(strict_types=1);

namespace Cairo {

    use Cairo\Internal\Go;

    final class Cache
    {
        /**
         * Drop the cached responses of every route whose ->cache() names one
         * of $tags, on all servers of this process. Call it when the data
         * behind those routes changes:
         *
         *     $http->get('/products', $list)->cache(ttl: 60, tags: ['products']);
         *     $http->post('/products', function (Request $r) use ($db): Response {
         *         $db->exec('INSERT ...');
         *         Cache::purge('products');
         *         return Response::json(['ok' => true], 201);
         *     });
         *
         * The next request for a purged route runs PHP again; a response PHP
         * was already building when the purge came is not stored. A tag no
         * route declares is ignored.
         */
        public static function purge(string ...$tags): void
        {
            if ($tags !== []) {
                Go::callNow('cache.purge', ['tags' => array_values($tags)]);
            }
        }
    }
}

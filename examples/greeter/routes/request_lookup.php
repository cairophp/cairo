<?php

// Request::header() finds a header whatever the case of its name, and
// Request::query() reads the query string, parsed once for the request.

declare(strict_types=1);

use Cairo\Http\Request;
use Cairo\Http\Response;

return function ($http): void {
    $http->get('/lookup', fn (Request $req): Response => Response::json([
        'canonical' => $req->header('X-Trace-Id'),
        'lower' => $req->header('x-trace-id'),
        'upper' => $req->header('X-TRACE-ID'),
        'missing' => $req->header('X-Not-Sent'),
        'page' => $req->query('page'),
        'sort' => $req->query('sort'),
        'page_again' => $req->query('page'),
        'list' => $req->query('tags'),
        'absent' => $req->query('absent'),
    ]));
};

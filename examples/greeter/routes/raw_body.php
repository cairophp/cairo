<?php

// Bodies cross between Go and PHP as raw bytes, both ways: any binary
// payload comes back unchanged.

declare(strict_types=1);

use Cairo\Http\Request;
use Cairo\Http\Response;

return function ($http): void {
    $http->post('/echo-body', fn (Request $req): Response => new Response(200, [
        'Content-Type' => 'application/octet-stream',
        'X-Body-Sha256' => hash('sha256', $req->body()),
    ], $req->body()));
};

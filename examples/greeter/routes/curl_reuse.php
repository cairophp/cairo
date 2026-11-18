<?php

// Hooked curl keeps connections alive: three curl_exec() calls on one handle
// open one connection between them, as they would outside Cairo. A handle
// made after another was closed takes over its connections too, which helps
// code that builds a new client for every call.

declare(strict_types=1);

use Cairo\Http\Request;
use Cairo\Http\Response;

return function ($http): void {
    $http->get('/curl/reuse', function (Request $req): Response {
        $ch = curl_init((string) $req->query('url'));
        curl_setopt($ch, CURLOPT_RETURNTRANSFER, true);
        $connects = [];
        for ($i = 0; $i < 3; $i++) {
            curl_exec($ch);
            $connects[] = curl_getinfo($ch, CURLINFO_NUM_CONNECTS);
        }
        return Response::json(['connects' => $connects]);
    });

    $http->get('/curl/reuse-across-handles', function (Request $req): Response {
        $connects = [];
        for ($i = 0; $i < 3; $i++) {
            $ch = curl_init((string) $req->query('url'));
            curl_setopt($ch, CURLOPT_RETURNTRANSFER, true);
            curl_exec($ch);
            $connects[] = curl_getinfo($ch, CURLINFO_NUM_CONNECTS);
            unset($ch);
        }
        return Response::json(['connects' => $connects]);
    });
};

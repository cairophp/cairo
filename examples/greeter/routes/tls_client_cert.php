<?php

// TLS with a client certificate (mutual TLS): the ssl context's local_cert
// and local_pk go to Go's crypto/tls.

declare(strict_types=1);

use Cairo\Http\Request;
use Cairo\Http\Response;

return function ($http): void {
    $http->get('/tls/client-cert', function (Request $req): Response {
        $ssl = ['cafile' => (string) $req->query('cafile')];
        if ($req->query('cert') !== null) {
            $ssl['local_cert'] = (string) $req->query('cert');
            $ssl['local_pk'] = (string) $req->query('key');
        }
        $body = @file_get_contents((string) $req->query('url'), false, stream_context_create(['ssl' => $ssl]));
        return Response::json(['body' => $body === false ? null : $body, 'error' => $body === false ? (error_get_last()['message'] ?? '') : null]);
    });
};

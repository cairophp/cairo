<?php

// curl on a busy server. /curl/open-fds counts the process's descriptors, so a
// test can push them past 1024 and check that curl still waits for its
// sockets instead of polling. /curl/failed-tasks runs async() tasks that fail
// while their exception holds a curl handle: Cairo collects that garbage when
// the request ends, so the handles' descriptors do not pile up.

declare(strict_types=1);

use Cairo\Http\Request;
use Cairo\Http\Response;

use function Cairo\async;
use function Cairo\await;

return function ($http): void {
    $http->get('/curl/open-fds', function (): Response {
        $fds = @scandir('/proc/self/fd');
        return Response::json([
            'open_fds' => $fds === false ? -1 : count($fds) - 2,
            'curl_version' => curl_version()['version_number'],
        ]);
    });

    $http->get('/curl/failed-tasks', function (Request $req): Response {
        $url = (string) $req->query('url');
        $fail = static function (\CurlHandle $ch): never {
            // The trace keeps $ch, and the task keeps the exception.
            throw new \RuntimeException('failed after ' . curl_getinfo($ch, CURLINFO_HTTP_CODE));
        };
        $tasks = [];
        for ($i = 0; $i < (int) $req->query('n'); $i++) {
            $tasks[] = async(static function () use ($url, $fail): void {
                $ch = curl_init($url);
                curl_setopt($ch, CURLOPT_RETURNTRANSFER, true);
                curl_exec($ch);
                $fail($ch);
            });
        }
        $failed = 0;
        foreach ($tasks as $task) {
            try {
                await($task);
            } catch (\RuntimeException) {
                $failed++;
            }
        }
        return Response::json(['failed' => $failed]);
    });
};

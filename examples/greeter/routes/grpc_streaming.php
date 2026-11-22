<?php

// Streaming gRPC from PHP: the generated client's streaming methods against
// this server's own streaming handlers (server.php).

declare(strict_types=1);

use Cairo\Grpc\Code;
use Cairo\Grpc\Exception as GrpcException;
use Cairo\Http\Request;
use Cairo\Http\Response;
use Helloworld\CountRequest;
use Helloworld\GreeterClient;
use Helloworld\HelloRequest;

return function ($http): void {
    $http->get('/grpc-stream/all', function (Request $req): Response {
        $greeter = new GreeterClient((string) $req->query('target'), ['timeout' => 10]);
        $out = [];

        // Server streaming: iterate the replies.
        foreach ($greeter->CountUp(new CountRequest(name: 'n', count: 3)) as $reply) {
            $out['count_up'][] = $reply->message;
        }

        // Client streaming: send names, then read the single reply.
        $join = $greeter->JoinNames();
        foreach (['Ada', 'Grace', 'Linus'] as $name) {
            $join->send(new HelloRequest(name: $name));
        }
        $out['join'] = $join->closeAndRecv()->message;

        // Bidirectional: one reply per message, as they arrive.
        $chat = $greeter->Chat();
        $chat->send(new HelloRequest(name: 'one'));
        $out['chat'][] = $chat->recv()->message;
        $chat->send(new HelloRequest(name: 'two'));
        $out['chat'][] = $chat->recv()->message;
        $chat->closeSend();
        $out['chat_end'] = $chat->recv();
        $out['has_trailers'] = is_array($chat->trailers());

        // A handler's exception ends the stream with its status.
        $bad = $greeter->Chat();
        $bad->send(new HelloRequest(name: ''));
        try {
            $bad->recv();
            $out['error'] = 'no exception';
        } catch (GrpcException $e) {
            $out['error'] = $e->getCode() === Code::INVALID_ARGUMENT ? 'invalid_argument' : (string) $e->getCode();
        }
        return Response::json($out);
    });

    // Replies arrive faster than they are read here, and Go reads them
    // ahead: none is lost, their order holds, and a status that ends the
    // stream comes after the replies before it.
    $http->get('/grpc-stream/ahead', function (Request $req): Response {
        $greeter = new GreeterClient((string) $req->query('target'), ['timeout' => 20]);
        $count = (int) $req->query('count');
        $read = 0;
        $ordered = true;
        foreach ($greeter->CountUp(new CountRequest(name: 'n', count: $count)) as $reply) {
            $ordered = $ordered && $reply->message === 'n ' . ++$read;
            if ($read === 1) {
                \Cairo\sleep(0.2);
            }
        }

        $chat = $greeter->Chat();
        foreach (['a', 'b', ''] as $name) {
            $chat->send(new HelloRequest(name: $name));
        }
        $chatReplies = [$chat->recv()->message];
        \Cairo\sleep(0.2);   // the second reply and the status are both there now
        try {
            while (($reply = $chat->recv()) !== null) {
                $chatReplies[] = $reply->message;
            }
            $chatReplies[] = 'no status';
        } catch (GrpcException $e) {
            $chatReplies[] = 'status ' . $e->getCode();
        }
        return Response::json(['read' => $read, 'ordered' => $ordered, 'chat' => $chatReplies]);
    });

    // A server stream that takes about 1s; many overlap on one PHP thread.
    $http->get('/grpc-stream/slow', function (Request $req): Response {
        $greeter = new GreeterClient((string) $req->query('target'));
        $messages = [];
        foreach ($greeter->CountUp(new CountRequest(name: 'slow', count: 3, interval: 0.5)) as $reply) {
            $messages[] = $reply->message;
        }
        return Response::json(['messages' => $messages]);
    });
};

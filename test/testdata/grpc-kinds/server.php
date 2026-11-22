<?php

// For TestGRPCMessageKinds: a service over messages with every kind of field
// (internal/pbwire/testdata/kinds.proto). It returns the arrays it gets, and
// says whether an array is the one json_decode() makes of protojson's text.
// With CAIRO_KINDS_UPSTREAM it passes each message on to the same service
// there and returns what comes back, so the messages also cross Cairo's
// gRPC client both ways.

declare(strict_types=1);

use Cairo\App;
use Cairo\Context;
use Cairo\Grpc\Client;
use Cairo\Grpc\ServerStream;

const KINDS_PROTO = __DIR__ . '/../../../internal/pbwire/testdata/kinds.proto';

final class Kinds
{
    /**
     * @param array<string, mixed> $request
     * @return array<string, mixed>
     */
    public function Echo(array $request, Context $ctx): array
    {
        $upstream = self::upstream();
        if ($upstream !== null) {
            return self::set($upstream->call('/kinds.Kinds/Echo', self::set($request)));
        }
        return self::reply($request);
    }

    /** @param ServerStream<array<string, mixed>, array<string, mixed>> $stream */
    public function EchoEach(ServerStream $stream, Context $ctx): void
    {
        $upstream = self::upstream()?->stream('/kinds.Kinds/EchoEach');
        foreach ($stream as $message) {
            if ($upstream !== null) {
                $upstream->send(self::set($message));
                $message = $upstream->recv() ?? throw new RuntimeException('the upstream ended its stream');
            }
            $stream->send(self::reply($message));
        }
        $upstream?->closeSend();
    }

    private static function upstream(): ?Client
    {
        $target = (string) getenv('CAIRO_KINDS_UPSTREAM');
        return $target === '' ? null : new Client($target, ['protos' => [KINDS_PROTO]]);
    }

    /**
     * @param array<string, mixed> $request
     * @return array<string, mixed>
     */
    public function Check(array $request, Context $ctx): array
    {
        $want = json_decode((string) $request['json'], true, 512, JSON_THROW_ON_ERROR);
        $same = $want === $request['message'];
        return ['same' => $same, 'got' => $same ? '' : var_export($request['message'], true)];
    }

    /**
     * The reply to a message: the message, or what is no message of its type
     * when the test asks for that.
     *
     * @param array<string, mixed> $message
     * @return array<int|string, mixed>
     */
    private static function reply(array $message): array
    {
        return $message['fString'] === 'reply with no message' ? ['fInt32' => 'x'] : self::set($message);
    }

    /**
     * The array to return for a message that arrived. A message arrives with
     * all of its fields, and PHP has one array for a list, a map and a
     * message, where protojson wants to know which: an unset list or map is
     * left out, and a map is made an object, as the classes that cairo gen
     * writes do. The maps of these messages are the fields named m_*; no
     * message of these types is an empty array, since each has a scalar.
     *
     * @param array<int|string, mixed> $message
     * @return array<int|string, mixed>
     */
    private static function set(array $message): array
    {
        $out = [];
        foreach ($message as $key => $value) {
            if ($value === null || $value === []) {
                continue;
            }
            if (is_array($value)) {
                $value = self::set($value);
                if (preg_match('/^m[A-Z]/', (string) $key) === 1) {
                    $value = (object) $value;
                }
            }
            $out[$key] = $value;
        }
        return $out;
    }
}

$app = new App();
$app->grpc(getenv('CAIRO_GRPC') ?: ':9000')
    ->proto(KINDS_PROTO)
    ->service('kinds.Kinds', Kinds::class);

return $app;

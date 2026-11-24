<?php

// mercure_publish(): the function FrankenPHP has for the same, so that code
// and libraries written for it publish on Cairo unchanged. Loaded on every
// PHP thread of a binary that has the hub (internal/sapi/mercure.go).

declare(strict_types=1);

namespace {

    /**
     * Publish an update to the Mercure hub of this process ($http->mercure()),
     * without HTTP and without a token. Only the calling fiber waits, for as
     * long as the hub takes to hand the update to its subscribers' queues; it
     * does not wait for the clients.
     *
     *     $id = mercure_publish('https://example.com/books/1', json_encode($book));
     *
     * @param string|list<string> $topics the topic, or the topic and its
     *        alternates: a subscriber to any of them gets the update
     * @param bool $private only for subscribers whose token names a topic of it
     * @param ?string $id the update's id; the hub makes one when null
     * @param ?string $type the event's name, for addEventListener()
     * @param ?int $retry milliseconds a client waits before it connects again
     * @return string the update's id, which a client sends as its last event id
     *         to get what came after it
     * @throws \ValueError for an update the protocol does not allow: a topic
     *         below /.well-known/mercure or "*", an id that starts with "#" or
     *         is "earliest", the type "mercure", data that is not UTF-8, a
     *         negative $retry
     * @throws \RuntimeException when no hub runs in this process (in a script
     *         of `cairo php`, for one), or the hub could not hand the update out
     */
    function mercure_publish(
        string|array $topics,
        string $data = '',
        bool $private = false,
        ?string $id = null,
        ?string $type = null,
        ?int $retry = null,
    ): string {
        $topics = (array) $topics;
        foreach ($topics as $topic) {
            if (!is_string($topic)) {
                throw new \TypeError('mercure_publish(): Argument #1 ($topics) must be of type string|array of strings');
            }
        }
        if ($retry !== null && $retry < 0) {
            throw new \ValueError('mercure_publish(): Argument #6 ($retry) must not be negative');
        }
        $published = \Cairo\Internal\Go::call('mercure.publish', [
            'topics' => array_values($topics),
            'data' => $data,
            'private' => $private,
            'id' => $id ?? '',
            'type' => $type ?? '',
            'retry' => $retry ?? 0,
        ]);
        if (isset($published['invalid'])) {
            throw new \ValueError('mercure_publish(): ' . $published['invalid']);
        }
        return $published['id'];
    }
}

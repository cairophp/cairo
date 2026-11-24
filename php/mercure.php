<?php

// Cairo\Http\Mercure: what $http->mercure() declares, the Mercure hub that
// Go serves (internal/mercurehub). Loaded on first use.

declare(strict_types=1);

namespace Cairo\Http {

    /**
     * The Mercure hub of an HTTP server, at /.well-known/mercure: browsers
     * subscribe to topics with an EventSource, and what is published to a
     * topic goes to all of its subscribers. PHP publishes with
     * mercure_publish(); other programs publish with a POST and a token.
     * Go serves every subscriber: no PHP thread and no fiber is held.
     *
     *     $http->mercure()
     *         ->issuer('https://example.com', publisher: $key, subscriber: $key)
     *         ->anonymous();
     *
     * The hub is the Mercure.rocks hub (github.com/dunglas/mercure), which
     * is licensed under the AGPL-3.0, not under Cairo's Apache-2.0. A cairo
     * built with the tag `nomercure` has none, and refuses this declaration.
     */
    final class Mercure
    {
        /** @var list<array{identifier: string, publisher: string, subscriber: string, algorithm: string}> */
        private array $issuers = [];
        /** @var array<string, mixed> */
        private array $spec = ['heartbeat' => 40.0, 'write_timeout' => 600.0, 'dispatch_timeout' => 5.0];

        /**
         * Trust the tokens of one issuer: $identifier is their `iss` claim.
         * $publisher verifies the token of a program that publishes with a
         * POST, $subscriber the token of a client that subscribes to
         * private updates; leave one empty and the hub takes no token of
         * that role. A key is a secret for the HS algorithms, or a public
         * key in PEM for the others (RS256, ES256, EdDSA and their kin).
         * Read keys from the environment, never from the code.
         *
         * With no publisher key at all, only mercure_publish() publishes.
         */
        public function issuer(string $identifier, string $publisher = '', string $subscriber = '', string $algorithm = 'HS256'): static
        {
            $this->issuers[] = ['identifier' => $identifier, 'publisher' => $publisher, 'subscriber' => $subscriber, 'algorithm' => $algorithm];
            return $this;
        }

        /** Let clients without a token subscribe. They get the updates that are not private. */
        public function anonymous(): static
        {
            return $this->with('anonymous', true);
        }

        /**
         * The sites whose pages may subscribe from a browser (CORS), as
         * origins: 'https://app.example.com', or '*' for any. Pages of the
         * hub's own origin need none.
         *
         * @param list<string> $origins
         */
        public function cors(array $origins): static
        {
            return $this->with('cors_origins', array_values($origins));
        }

        /**
         * The sites whose pages may publish with the token cookie. A program
         * that sends its token in the Authorization header needs none.
         *
         * @param list<string> $origins
         */
        public function publishOrigins(array $origins): static
        {
            return $this->with('publish_origins', array_values($origins));
        }

        /**
         * Keep the updates in $file, so that a client which comes back with
         * its last event id gets what it missed, also after a restart.
         * $size is how many updates are kept, 0 for all of them. Without
         * this call nothing is kept: a client gets what is published while
         * it listens.
         */
        public function history(string $file, int $size = 0): static
        {
            return $this->with('history', $file)->with('history_size', max(0, $size));
        }

        /** Turn on the subscription API and the events that tell who subscribes to what. */
        public function subscriptions(): static
        {
            return $this->with('subscriptions', true);
        }

        /**
         * Also take the clients and tokens of the protocol before Mercure
         * 1.0 (`?topic=`, the `mercure` claim), for the time it takes to
         * move them: $version is 8, or 7 for the oldest. It relaxes how
         * tokens are checked, so turn it off again when the clients moved.
         */
        public function compatibility(int $version = 8): static
        {
            return $this->with('compatibility', $version);
        }

        /**
         * The audience (`aud`) that tokens must name, as an absolute URL.
         * Without it, it is the hub's URL as the client asked for it,
         * which is not the public one behind a proxy that ends TLS: say it
         * there ('https://example.com/.well-known/mercure').
         */
        public function resource(string $identifier): static
        {
            return $this->with('resource', $identifier);
        }

        /**
         * Answer only for these public URLs of the server (scheme and
         * host); a request for another host gets 421.
         *
         * @param list<string> $urls
         */
        public function publicUrls(array $urls): static
        {
            return $this->with('public_urls', array_values($urls));
        }

        /**
         * Seconds. $heartbeat: a comment goes to a quiet subscriber that
         * often, so that no proxy closes its connection. $write: a
         * subscriber is closed after that long, and connects again.
         * $dispatch: how long one update may take to reach a subscriber
         * before the subscriber is dropped. 0 turns the first two off.
         */
        public function timeouts(float $heartbeat = 40.0, float $write = 600.0, float $dispatch = 5.0): static
        {
            return $this->with('heartbeat', $heartbeat)->with('write_timeout', $write)->with('dispatch_timeout', $dispatch);
        }

        /**
         * How many subscriptions may be open at once: in all, with one
         * token, and from one client address. 0 is no limit.
         */
        public function limits(int $total = 0, int $perToken = 0, int $perClient = 0): static
        {
            return $this->with('max_subscriptions', $total)
                ->with('max_subscriptions_per_token', $perToken)
                ->with('max_subscriptions_per_client', $perClient);
        }

        /**
         * The name of the cookie a browser sends its token in. The default,
         * `__Secure-mercure_access_token`, only travels over HTTPS: give a
         * name without that prefix for development over plain HTTP.
         */
        public function cookie(string $name): static
        {
            return $this->with('cookie_name', $name);
        }

        /** Serve the hub's debugging page at /.well-known/mercure/debug/. Not for production. */
        public function debugger(): static
        {
            return $this->with('debugger', true);
        }

        private function with(string $key, mixed $value): static
        {
            $this->spec[$key] = $value;
            return $this;
        }

        /**
         * @internal
         * @return array<string, mixed>
         */
        public function spec(): array
        {
            return $this->spec + ['issuers' => $this->issuers];
        }
    }
}

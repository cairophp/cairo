<?php
/**
 * What a script served in C (internal/sapi/plain_request.c) has of Cairo
 * before it runs, loaded in each of its PHP requests: the functions of the
 * concurrency API (async(), await(), all(), sleep(), select(), offload()).
 * Their classes load on first use, and with them the whole runtime, with a
 * context for the request and a scheduler for what waits: the script uses
 * Cairo, and from then on runs as a classic request of the runtime does.
 */

declare(strict_types=1);

namespace Cairo\Internal {

    use Cairo\App;
    use Cairo\Context;

    require_once __DIR__ . '/async.php';
    require_once __DIR__ . '/channel.php';
    require_once __DIR__ . '/offload.php';
    spl_autoload_register(PlainRequest::upgrade(...));

    /** @internal */
    final class PlainRequest
    {
        private static bool $loaded = false;
        private static ?Context $context = null;

        /** A Cairo class that is not loaded: the runtime loads. */
        public static function upgrade(string $class): void
        {
            if (self::$loaded || !str_starts_with($class, 'Cairo\\')) {
                return;
            }
            self::$loaded = true;
            $info = json_decode(\cairo_info(), true, 512, JSON_THROW_ON_ERROR);
            require_once __DIR__ . '/loader.php';
            load($info);
            autoload($info);
            /** @var array{id: int, deadline: ?float, values: array<string, mixed>} $call */
            $call = \cairo_plain_call();
            self::$context = new Context($call['id'], $call['deadline']);
            if ($call['values'] !== []) {
                self::$context->setGoValues($call['values']);
            }
            // As a classic request of the runtime has it (Scheduler::start()):
            // the calls that come meanwhile wait for the thread's next PHP
            // request, and this one runs on the thread's own stack.
            \cairo_hold_calls();
            $tick = self::$context->deadlineTick();
            \cairo_fiber_bind(null, self::$context, $call['id'], $tick === PHP_INT_MAX ? 0 : $tick);
            // The scheduler only waits here: no server of its App takes a
            // request (the declaration's, if any, stays in Go).
            new Scheduler((new App())->concurrency('classic'));
        }

        /** @internal for plain_request.c: the request has its answer */
        public static function answered(): void
        {
            self::$context?->markAnswered();
        }

        /** @internal for plain_request.c, once the script is answered: what the context still holds runs */
        public static function end(): void
        {
            $context = self::$context;
            if ($context === null) {
                return;
            }
            self::$context = null;
            try {
                $context->finish();
            } finally {
                $context->ended('canceled');
                \cairo_local_end($context->requestId);
                \cairo_fiber_bind(null, null, 0);
            }
        }
    }
}

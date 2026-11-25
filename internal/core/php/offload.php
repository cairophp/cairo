<?php
/**
 * offload(): CPU-heavy work on the offload threads, and the loop those
 * threads run.
 */

declare(strict_types=1);

namespace Cairo {

    use Cairo\Internal\Scheduler;

    /**
     * A job for offload() that carries its own input: the object is
     * serialized, sent to an offload thread and run() there. Its class must be
     * loadable on every thread (declared or autoloaded from server.php).
     *
     *     final class Thumbnail implements OffloadTask {
     *         public function __construct(private string $path, private int $width) {}
     *         public function run(): string { ... }
     *     }
     *     $png = offload(new Thumbnail($path, 200));
     */
    interface OffloadTask
    {
        public function run(): mixed;
    }

    /**
     * Run CPU-heavy work on the offload thread pool, so this request thread
     * keeps serving other fibers. The calling fiber suspends until it is done.
     *
     * Code cannot move between PHP threads, so $fn must be an OffloadTask
     * object, a function name or [ClassName::class, 'staticMethod'] that
     * server.php makes available, and the task, $args and the result must be
     * serializable. Closures are not accepted.
     *
     * @param OffloadTask|callable-string|array{0:class-string,1:string} $fn
     */
    function offload(OffloadTask|string|array $fn, mixed ...$args): mixed
    {
        if ($fn instanceof OffloadTask && $args !== []) {
            throw new \InvalidArgumentException('offload() takes no arguments with an OffloadTask; give them to the task');
        }
        if (\Fiber::getCurrent() === null || !Scheduler::running() || Scheduler::instance()->script) {
            if (Scheduler::onStack()) {
                return \Cairo\await(\Cairo\async(static fn (): mixed => offload($fn, ...$args)));
            }
            // already off the request threads, or in a script, which has no offload threads
            return $fn instanceof OffloadTask ? $fn->run() : $fn(...$args);
        }
        if (!$fn instanceof OffloadTask && !is_callable($fn)) {
            throw new \InvalidArgumentException('offload() needs an OffloadTask, a function name or [Class::class, "staticMethod"]');
        }
        $out = unserialize(Scheduler::instance()->jobs->run(serialize([$fn, $args])));
        if (!($out['ok'] ?? false)) {
            throw new OffloadException(($out['class'] ?? 'Error') . ': ' . ($out['message'] ?? 'offloaded job failed'));
        }
        return $out['value'];
    }

    /**
     * An offloaded job threw, and the message names the original exception
     * class; or the offload thread stopped before the job was done.
     */
    final class OffloadException extends \RuntimeException
    {
    }
}

namespace Cairo\Internal {

    use Cairo\Context;
    use Cairo\OffloadException;

    /**
     * The offload() jobs that fibers of one request thread wait for. A job
     * goes straight into the inbox of an offload thread and its result comes
     * straight back into this thread's (cairo_offload_send() and
     * cairo_offload_done() in offload.c); Go has no part in it.
     */
    final class Jobs
    {
        /** @var array<int, Waiter> job id => the fiber that waits for its result */
        private array $waiting = [];

        public function __construct(private readonly Waits $waits)
        {
        }

        /**
         * Send $job, a serialized [$fn, $args], to an offload thread and
         * suspend the current fiber until its serialized result is here.
         * Throws when the fiber's context ends first: the job then runs on,
         * and its result finds nobody.
         */
        public function run(string $job): string
        {
            $ctx = Context::current();
            if ($ctx->requestId === 0) {
                throw new \LogicException('Cairo I/O must run inside a request handler or an async() task');
            }
            $ctx->throwIfDone();
            while (($id = \cairo_offload_send($job, $ctx->requestId)) === 0) {
                $this->waits->sleep(0.001);   // every offload thread has a full queue
            }
            $this->waiting[$id] = $waiter = new Waiter();
            try {
                $ended = $this->waits->park($ctx, -1.0, $waiter, Waits::OFFLOAD);
            } finally {
                unset($this->waiting[$id]);
            }
            if ($waiter->error !== null) {
                throw $waiter->error;
            }
            if (!$waiter->done) {
                throw Waits::exceptionOf($ctx, (string) $ended);
            }
            return (string) $waiter->result[1];
        }

        /**
         * The loop took an 'offloaded' event: the result of job `id`, or the
         * `error` that it has none. A job whose fiber no longer waits, or
         * that a thread before this one sent, is not found.
         *
         * @param array{id: int, result?: string, error?: string} $event
         */
        public function done(array $event): void
        {
            $waiter = $this->waiting[$event['id']] ?? null;
            if (isset($event['error'])) {
                $waiter?->fail(new OffloadException($event['error']));
            } else {
                $waiter?->wake(0, $event['result'], true);
            }
        }
    }

    /** Runs Cairo\offload() jobs, one at a time, on an offload thread. */
    final class OffloadWorker
    {
        public function run(): void
        {
            while (true) {
                $event = \cairo_next_event(true);   // null: woken for nothing
                $type = $event['type'] ?? null;
                if ($type === 'shutdown') {
                    return;
                }
                if ($type !== 'offload') {
                    continue;
                }
                try {
                    [$fn, $args] = unserialize($event['payload']);
                    $out = serialize(['ok' => true, 'value' => $fn instanceof \Cairo\OffloadTask ? $fn->run() : $fn(...$args)]);
                } catch (\Throwable $e) {
                    $out = serialize(['ok' => false, 'class' => $e::class, 'message' => $e->getMessage()]);
                }
                \cairo_offload_done($out);
            }
        }
    }
}

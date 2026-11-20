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
        if (\Fiber::getCurrent() === null || !Scheduler::running()) {
            // already off the request threads
            return $fn instanceof OffloadTask ? $fn->run() : $fn(...$args);
        }
        if (!$fn instanceof OffloadTask && !is_callable($fn)) {
            throw new \InvalidArgumentException('offload() needs an OffloadTask, a function name or [Class::class, "staticMethod"]');
        }
        $payload = base64_encode(serialize([$fn, $args]));
        $result = Scheduler::instance()->call('offload', $payload);
        $out = unserialize(base64_decode((string) $result));
        if (!($out['ok'] ?? false)) {
            throw new OffloadException(($out['class'] ?? 'Error') . ': ' . ($out['message'] ?? 'offloaded job failed'));
        }
        return $out['value'];
    }

    /** An offloaded job threw; the message names the original exception class. */
    final class OffloadException extends \RuntimeException
    {
    }
}

namespace Cairo\Internal {

    /** Runs Cairo\offload() jobs, one at a time, on an offload thread. */
    final class OffloadWorker
    {
        public function run(): void
        {
            while (true) {
                $event = \cairo_next_event(-1);   // null: woken for nothing
                $type = $event['type'] ?? null;
                if ($type === 'shutdown') {
                    return;
                }
                if ($type !== 'offload') {
                    continue;
                }
                try {
                    [$fn, $args] = unserialize(base64_decode($event['payload']));
                    $out = ['ok' => true, 'value' => $fn instanceof \Cairo\OffloadTask ? $fn->run() : $fn(...$args)];
                } catch (\Throwable $e) {
                    $out = ['ok' => false, 'class' => $e::class, 'message' => $e->getMessage()];
                }
                \cairo_respond((int) $event['id'], json_encode(['payload' => base64_encode(serialize($out))], JSON_THROW_ON_ERROR));
            }
        }
    }
}

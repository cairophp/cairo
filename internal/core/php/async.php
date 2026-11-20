<?php
/**
 * async(), await() and all(): the fibers a request starts next to its
 * own. And sleep(), the wait that suspends only the fiber.
 */

declare(strict_types=1);

namespace Cairo {

    use Cairo\Internal\Scheduler;

    /** A unit of concurrent work started with async(). */
    final class Task
    {
        /** @internal */ public bool $done = false;
        /** @internal */ public mixed $result = null;
        /** @internal */ public ?\Throwable $error = null;
        /** @internal @var list<\Fiber> */ public array $waiters = [];
    }

    /**
     * Run $fn concurrently in its own fiber, under the current context.
     * It runs until its first suspension, then async() returns.
     */
    function async(callable $fn): Task
    {
        return Internal\spawnTask(Context::current(), $fn);
    }

    /** Wait for a task; rethrows its exception. */
    function await(Task $task): mixed
    {
        if (!$task->done) {
            $fiber = \Fiber::getCurrent() ?? throw new \LogicException('await() must be called inside a request (a fiber)');
            $task->waiters[] = $fiber;
            \Fiber::suspend();
        }
        if ($task->error !== null) {
            throw $task->error;
        }
        return $task->result;
    }

    /**
     * A task that finishes when all $tasks finish, with their results in the
     * same order (keys kept).
     * @param array<Task> $tasks
     */
    function all(array $tasks): Task
    {
        return async(static function () use ($tasks): array {
            $results = [];
            foreach ($tasks as $key => $task) {
                $results[$key] = await($task);
            }
            return $results;
        });
    }

    /**
     * Sleep without blocking the thread: the fiber suspends, and the thread
     * runs other fibers until the time is up. Throws
     * Context\DeadlineExceeded or Context\Canceled if the request's context
     * ends first.
     */
    function sleep(float $seconds): void
    {
        if (\Fiber::getCurrent() === null) {
            usleep((int) ($seconds * 1e6));
            return;
        }
        Scheduler::instance()->waits->sleep($seconds);
    }
}

namespace Cairo\Internal {

    use Cairo\Context;

/** @internal async(), with the parent context given: start $fn in a new fiber with its own goroutine. */
    function spawnTask(Context $parent, callable $fn): \Cairo\Task
    {
        $task = new \Cairo\Task();
        // Go starts the goroutine that will serve this fiber.
        $goroutine = \cairo_fiber_open($parent->fiberId);
        $context = $parent->child($goroutine);
        Scheduler::instance()->startFiber(static function () use ($fn, $task, $goroutine, $context): void {
            try {
                $task->result = \cairo_guard($fn);
            } catch (\Throwable $e) {
                $task->error = $e;
                Scheduler::instance()->taskFailed();
            } finally {
                $context->finish();
                \cairo_fiber_close($goroutine);
            }
            $task->done = true;
            foreach ($task->waiters as $waiter) {
                Scheduler::instance()->ready($waiter);
            }
            $task->waiters = [];
        }, $context);
        return $task;
    }
}

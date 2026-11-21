<?php
/**
 * Cairo's driver for Revolt (revolt/event-loop), the event loop under Amp v3
 * and ReactPHP's fiber adapters. Cairo loads this file on first use of a
 * Cairo\Revolt\ class, and revolt/event-loop must be loadable by then:
 *
 *     Cairo\Revolt\Driver::install();   // in server.php, after vendor/autoload.php
 *
 * Revolt's own drivers run a loop in a fiber of their own. Under Cairo every
 * request and async() task already is a fiber whose waits Cairo serves, so
 * this driver maps Revolt onto Cairo instead of running a second loop:
 *
 * - each callback runs in a Cairo async() task: defer() and queue() as soon as
 *   the fiber that made them yields, delay() and repeat() after a timer of
 *   the thread, onReadable() and onWritable() when the thread's loop finds
 *   the socket ready;
 * - a Suspension parks its fiber on the thread until resume() or throw().
 *
 * A callback belongs to the fiber that made it, so it ends with that request,
 * as everything else the request started does. onSignal() is not supported:
 * Go handles the process's signals.
 */

declare(strict_types=1);

namespace Cairo\Revolt;

use Cairo\Context;
use Cairo\Internal\Scheduler;
use Cairo\Internal\Waiter;

use function Cairo\Internal\spawnTask;
use Revolt\EventLoop;
use Revolt\EventLoop\CallbackType;
use Revolt\EventLoop\FiberLocal;
use Revolt\EventLoop\InvalidCallbackError;
use Revolt\EventLoop\UncaughtThrowable;
use Revolt\EventLoop\UnsupportedFeatureException;

// Driver implements Revolt's interface, so nothing here can be declared
// without it.
if (!interface_exists(EventLoop\Driver::class)) {
    throw new \LogicException('Cairo\Revolt\Driver needs revolt/event-loop: composer require revolt/event-loop, and load vendor/autoload.php first');
}

/** @internal one Revolt callback */
final class Callback
{
    public bool $enabled = true;
    public bool $referenced = true;
    /** Bumped by every enable() and disable(): a runner from before stops. */
    public int $generation = 0;
    /** A delay() or repeat() runner parked on its timer. */
    public ?Waiter $timer = null;

    /** @param resource|null $stream */
    public function __construct(
        public readonly string $id,
        public readonly CallbackType $type,
        public readonly \Closure $closure,
        public readonly float $interval = 0.0,
        public readonly mixed $stream = null,
        public readonly int $fd = -1,
    ) {
    }
}

/**
 * @internal the onReadable()/onWritable() callbacks of one socket. One task
 * waits for all of them at once, since a socket has one waiter at a time
 * (Waits::sockets()).
 */
final class Watch
{
    /** @var array<string, Callback> */
    public array $callbacks = [];
    /** The waiting task's context: canceled when what it waits for changes. */
    public ?Context $context = null;
    public bool $running = false;

    /** @param resource $stream */
    public function __construct(public readonly int $fd, public readonly mixed $stream)
    {
    }
}

final class Driver implements EventLoop\Driver
{
    /** @var array<string, Callback> */
    private array $callbacks = [];
    /** @var array<int, Watch> by descriptor */
    private array $watches = [];
    private int $nextId = 0;
    private ?\Closure $errorHandler = null;
    /** @var \WeakMap<\Fiber, Suspension> */
    private \WeakMap $suspensions;
    private bool $running = false;
    private bool $stopped = false;
    private ?Waiter $runWaiter = null;

    public function __construct()
    {
        $this->suspensions = new \WeakMap();
    }

    /** Make this driver Revolt's (and so Amp's) event loop on this PHP thread. */
    public static function install(): self
    {
        $driver = new self();
        EventLoop::setDriver($driver);
        return $driver;
    }

    /**
     * Wait until no referenced callback is left, or stop() is called. Rarely
     * needed under Cairo, where the scheduler always runs.
     */
    public function run(): void
    {
        if ($this->running) {
            throw new \Error('The event loop is already running');
        }
        $this->running = true;
        $this->stopped = false;
        try {
            while (!$this->stopped && $this->referenced()) {
                $this->runWaiter = new Waiter();
                $this->park($this->runWaiter, -1.0);
            }
        } finally {
            $this->running = false;
            $this->runWaiter = null;
        }
    }

    public function stop(): void
    {
        $this->stopped = true;
        $this->runWaiter?->wake(0, null, true);
    }

    public function isRunning(): bool
    {
        return $this->running;
    }

    public function getSuspension(): EventLoop\Suspension
    {
        $fiber = \Fiber::getCurrent() ?? throw new \Error('Cairo\Revolt\Driver: suspend inside a request or an async() task');
        return $this->suspensions[$fiber] ??= new Suspension($fiber);
    }

    public function queue(\Closure $closure, mixed ...$args): void
    {
        $this->spawn(function () use ($closure, $args): void {
            try {
                $closure(...$args);
            } catch (\Throwable $e) {
                $this->error($closure, $e);
            } finally {
                FiberLocal::clear();
            }
        });
    }

    public function defer(\Closure $closure): string
    {
        return $this->add(new Callback($this->id(), CallbackType::Defer, $closure));
    }

    public function delay(float $delay, \Closure $closure): string
    {
        if ($delay < 0) {
            throw new \Error('Delay must be greater than or equal to zero');
        }
        return $this->add(new Callback($this->id(), CallbackType::Delay, $closure, $delay));
    }

    public function repeat(float $interval, \Closure $closure): string
    {
        if ($interval < 0) {
            throw new \Error('Interval must be greater than or equal to zero');
        }
        return $this->add(new Callback($this->id(), CallbackType::Repeat, $closure, $interval));
    }

    public function onReadable(mixed $stream, \Closure $closure): string
    {
        return $this->add(new Callback($this->id(), CallbackType::Readable, $closure, 0.0, $stream, self::fd($stream)));
    }

    public function onWritable(mixed $stream, \Closure $closure): string
    {
        return $this->add(new Callback($this->id(), CallbackType::Writable, $closure, 0.0, $stream, self::fd($stream)));
    }

    public function onSignal(int $signal, \Closure $closure): string
    {
        throw new UnsupportedFeatureException('Cairo does not pass signals to PHP: Go handles them');
    }

    public function enable(string $callbackId): string
    {
        $cb = $this->callbacks[$callbackId] ?? throw InvalidCallbackError::invalidIdentifier($callbackId);
        if (!$cb->enabled) {
            $cb->enabled = true;
            $cb->generation++;
            $this->activate($cb);
        }
        return $callbackId;
    }

    public function disable(string $callbackId): string
    {
        $cb = $this->callbacks[$callbackId] ?? null;
        if ($cb !== null && $cb->enabled) {
            $cb->enabled = false;
            $cb->generation++;
            $this->deactivate($cb);
            $this->changed();
        }
        return $callbackId;
    }

    public function cancel(string $callbackId): void
    {
        $this->disable($callbackId);
        $cb = $this->callbacks[$callbackId] ?? null;
        if ($cb === null) {
            return;
        }
        unset($this->callbacks[$callbackId]);
        if ($cb->fd >= 0 && isset($this->watches[$cb->fd])) {
            unset($this->watches[$cb->fd]->callbacks[$callbackId]);
        }
        $this->changed();
    }

    public function reference(string $callbackId): string
    {
        $cb = $this->callbacks[$callbackId] ?? throw InvalidCallbackError::invalidIdentifier($callbackId);
        $cb->referenced = true;
        return $callbackId;
    }

    public function unreference(string $callbackId): string
    {
        if (isset($this->callbacks[$callbackId])) {
            $this->callbacks[$callbackId]->referenced = false;
            $this->changed();
        }
        return $callbackId;
    }

    public function setErrorHandler(?\Closure $errorHandler): void
    {
        $this->errorHandler = $errorHandler;
    }

    public function getErrorHandler(): ?\Closure
    {
        return $this->errorHandler;
    }

    public function getHandle(): mixed
    {
        return null;
    }

    public function getIdentifiers(): array
    {
        return array_map('strval', array_keys($this->callbacks));
    }

    public function getType(string $callbackId): CallbackType
    {
        return ($this->callbacks[$callbackId] ?? throw InvalidCallbackError::invalidIdentifier($callbackId))->type;
    }

    public function isEnabled(string $callbackId): bool
    {
        return ($this->callbacks[$callbackId] ?? throw InvalidCallbackError::invalidIdentifier($callbackId))->enabled;
    }

    public function isReferenced(string $callbackId): bool
    {
        return ($this->callbacks[$callbackId] ?? throw InvalidCallbackError::invalidIdentifier($callbackId))->referenced;
    }

    public function __debugInfo(): array
    {
        $count = static fn (bool $enabled, bool $referenced): int => count(array_filter(
            $this->callbacks,
            static fn (Callback $cb): bool => $cb->enabled === $enabled && $cb->referenced === $referenced,
        ));
        return [
            'enabled_watchers' => ['referenced' => $count(true, true), 'unreferenced' => $count(true, false)],
            'disabled_watchers' => ['referenced' => $count(false, true), 'unreferenced' => $count(false, false)],
        ];
    }

    // ---- internals ----

    private function id(): string
    {
        return 'c' . $this->nextId++;
    }

    /** @param resource $stream */
    private static function fd(mixed $stream): int
    {
        if (!is_resource($stream)) {
            throw new \TypeError('Cairo\Revolt\Driver: expected a stream resource');
        }
        $fd = \cairo_stream_fd($stream);
        if ($fd < 0) {
            throw new UnsupportedFeatureException('Cairo\Revolt\Driver: this stream has no socket or pipe descriptor to wait on');
        }
        return $fd;
    }

    private function add(Callback $cb): string
    {
        $this->callbacks[$cb->id] = $cb;
        if ($cb->fd >= 0) {
            $watch = $this->watches[$cb->fd] ??= new Watch($cb->fd, $cb->stream);
            $watch->callbacks[$cb->id] = $cb;
        }
        $this->activate($cb);
        return $cb->id;
    }

    /** Start what makes an enabled callback run. */
    private function activate(Callback $cb): void
    {
        if ($cb->fd >= 0) {
            $this->rewatch($this->watches[$cb->fd]);
            return;
        }
        $generation = $cb->generation;
        $this->spawn(fn () => $this->runTimer($cb, $generation));
    }

    /** Stop what would make a callback run. */
    private function deactivate(Callback $cb): void
    {
        if ($cb->fd >= 0) {
            if (isset($this->watches[$cb->fd])) {
                $this->rewatch($this->watches[$cb->fd]);
            }
            return;
        }
        $cb->timer?->wake(0, null, true);
    }

    private function live(Callback $cb, int $generation): bool
    {
        return $cb->enabled && $cb->generation === $generation && isset($this->callbacks[$cb->id]);
    }

    /** A defer(), delay() or repeat() callback, in its own task. */
    private function runTimer(Callback $cb, int $generation): void
    {
        do {
            if ($cb->type !== CallbackType::Defer) {
                $cb->timer = $waiter = new Waiter();
                try {
                    $this->park($waiter, $cb->interval);
                } finally {
                    $cb->timer = null;
                }
            }
            if (!$this->live($cb, $generation)) {
                return;
            }
            if ($cb->type !== CallbackType::Repeat) {
                $this->cancel($cb->id);   // a one-off is gone before it runs, as in Revolt
            }
            $this->invoke($cb);
        } while ($cb->type === CallbackType::Repeat && $this->live($cb, $generation));
    }

    /**
     * The set of directions a socket is watched for changed: end the task
     * waiting on it (it was waiting for the old set) and start one for the
     * new set.
     */
    private function rewatch(Watch $watch): void
    {
        $watch->context?->cancel();
        $watch->context = null;
        if (!$watch->running && $this->wanted($watch) !== [false, false]) {
            $watch->running = true;
            $this->spawn(fn () => $this->runWatch($watch));
        }
    }

    /** @return array{bool, bool} whether an enabled callback wants reads, writes */
    private function wanted(Watch $watch): array
    {
        $read = $write = false;
        foreach ($watch->callbacks as $cb) {
            if ($cb->enabled) {
                $read = $read || $cb->type === CallbackType::Readable;
                $write = $write || $cb->type === CallbackType::Writable;
            }
        }
        return [$read, $write];
    }

    /** Waits on one socket for its callbacks, until none is enabled. */
    private function runWatch(Watch $watch): void
    {
        try {
            while (true) {
                [$read, $write] = $this->wanted($watch);
                if (!$read && !$write) {
                    return;
                }
                if (!is_resource($watch->stream)) {
                    return;   // closed without canceling its callbacks first
                }
                // PHP may already hold buffered data, which the kernel does not report.
                [$canRead, $canWrite] = $this->ready($watch->stream, $read, $write);
                if (!$canRead && !$canWrite) {
                    $context = $watch->context = Context::current()->withCancel();
                    try {
                        Scheduler::instance()->waits->sockets($context, $read ? [$watch->fd] : [], $write ? [$watch->fd] : [], -1.0);
                    } catch (\Cairo\Context\Canceled $e) {
                        if ($watch->context === $context) {
                            throw $e;   // not rewatch(): the request itself ended
                        }
                        // rewatch() canceled it: look at what is wanted now.
                    } finally {
                        if ($watch->context === $context) {
                            $watch->context = null;
                        }
                    }
                    if (!is_resource($watch->stream)) {
                        return;
                    }
                    [$canRead, $canWrite] = $this->ready($watch->stream, $read, $write);
                }
                foreach ($watch->callbacks as $cb) {
                    $due = ($cb->type === CallbackType::Readable && $canRead) || ($cb->type === CallbackType::Writable && $canWrite);
                    if ($due && $cb->enabled && isset($this->callbacks[$cb->id])) {
                        $this->invoke($cb);
                    }
                }
            }
        } finally {
            $watch->running = false;
            if ($watch->callbacks === []) {
                unset($this->watches[$watch->fd]);
            }
        }
    }

    /**
     * @param resource $stream
     * @return array{bool, bool} readable, writable now
     */
    private function ready(mixed $stream, bool $read, bool $write): array
    {
        $r = $read ? [$stream] : [];
        $w = $write ? [$stream] : [];
        $e = [];
        if (($r === [] && $w === []) || @stream_select($r, $w, $e, 0) === false) {
            return [false, false];
        }
        return [$r !== [], $w !== []];
    }

    private function invoke(Callback $cb): void
    {
        try {
            $result = match ($cb->type) {
                CallbackType::Readable, CallbackType::Writable => ($cb->closure)($cb->id, $cb->stream),
                default => ($cb->closure)($cb->id),
            };
            if ($result !== null) {
                throw InvalidCallbackError::nonNullReturn($cb->id, $cb->closure);
            }
        } catch (\Throwable $e) {
            $this->error($cb->closure, $e);
        } finally {
            FiberLocal::clear();
        }
    }

    /**
     * A callback threw. With an error handler, it gets the exception. Without
     * one, Revolt would stop the loop; Cairo has no loop of Revolt's to stop,
     * so it logs the exception instead.
     */
    private function error(\Closure $closure, \Throwable $e): void
    {
        if ($this->errorHandler === null) {
            error_log('cairo: ' . UncaughtThrowable::throwingCallback($closure, $e));
            return;
        }
        try {
            ($this->errorHandler)($e);
        } catch (\Throwable $e2) {
            error_log('cairo: ' . UncaughtThrowable::throwingErrorHandler($this->errorHandler, $e2));
        }
    }

    /**
     * Run $fn in a new async() task, from the scheduler's loop: Revolt never
     * runs a callback inside the call that scheduled it, and libraries
     * schedule from destructors, where PHP cannot switch fibers. The task is
     * a child of the request, not of the fiber scheduling it: a callback that
     * schedules another must not take it down when it returns.
     */
    private function spawn(\Closure $fn): void
    {
        $root = Context::current()->root();
        if (!Scheduler::running() || $root->requestId === 0) {
            throw new \Error('Cairo\Revolt\Driver: callbacks can only be scheduled inside a request or an async() task');
        }
        Scheduler::instance()->later(static function () use ($root, $fn): void {
            spawnTask($root, $fn);
        });
    }

    /**
     * Park the current fiber until $waiter is woken (true) or $timeout
     * seconds pass (false; < 0 for none). Throws when the fiber's context ends.
     */
    private function park(Waiter $waiter, float $timeout): bool
    {
        $ctx = Context::current();
        $ended = Scheduler::instance()->waits->park($ctx, $timeout, $waiter);
        if ($waiter->done) {
            if ($waiter->error !== null) {
                throw $waiter->error;
            }
            return true;
        }
        if ($ended !== null) {
            $ctx->ended($ended);
            throw $ended === 'canceled' ? new \Cairo\Context\Canceled() : new \Cairo\Context\DeadlineExceeded();
        }
        return false;
    }

    private function referenced(): bool
    {
        foreach ($this->callbacks as $cb) {
            if ($cb->enabled && $cb->referenced) {
                return true;
            }
        }
        return false;
    }

    /** Callbacks went away: run() may be done. */
    private function changed(): void
    {
        if ($this->runWaiter !== null && !$this->referenced()) {
            $this->runWaiter->wake(0, null, true);
        }
    }
}

/** Revolt's Suspension, on Cairo: the fiber parks in Go until resume() or throw(). */
final class Suspension implements EventLoop\Suspension
{
    private ?Waiter $waiter = null;
    /** @var \WeakReference<\Fiber> */
    private readonly \WeakReference $fiber;

    public function __construct(\Fiber $fiber)
    {
        $this->fiber = \WeakReference::create($fiber);
    }

    public function suspend(): mixed
    {
        if ($this->waiter !== null) {
            throw new \Error('Must call resume() or throw() before calling suspend() again');
        }
        if ($this->fiber->get() !== \Fiber::getCurrent()) {
            throw new \Error('Must not call suspend() from another fiber');
        }
        $waiter = $this->waiter = new Waiter();
        $ctx = Context::current();
        try {
            $ended = Scheduler::instance()->waits->park($ctx, -1.0, $waiter) ?? 'canceled';
        } finally {
            $this->waiter = null;
        }
        if ($waiter->done) {
            if ($waiter->error !== null) {
                throw $waiter->error;
            }
            return $waiter->result[1];
        }
        $ctx->ended($ended);
        throw $ended === 'deadline_exceeded' ? new \Cairo\Context\DeadlineExceeded() : new \Cairo\Context\Canceled();
    }

    public function resume(mixed $value = null): void
    {
        $waiter = $this->pending('resume');
        $this->waiter = null;
        $waiter->wake(0, $value, true);
    }

    public function throw(\Throwable $throwable): void
    {
        $waiter = $this->pending('throw');
        $this->waiter = null;
        $waiter->fail($throwable);
    }

    private function pending(string $method): Waiter
    {
        if ($this->waiter === null || $this->waiter->done) {
            throw new \Error("Must call suspend() before calling $method()");
        }
        return $this->waiter;
    }
}

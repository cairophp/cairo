<?php
/**
 * Signals in a script of `cairo php`, with PHP's own pcntl functions: what a
 * queue worker or a console command does to stop cleanly, to give a job a
 * time limit, or to report on demand.
 *
 *   cairo php examples/cli/signals.php worker      (kill -TERM stops it after the job in hand)
 *   cairo php examples/cli/signals.php busy        (a handler runs in code that never waits)
 *   cairo php examples/cli/signals.php tasks 50    (a handler runs while 50 fibers wait)
 *   cairo php examples/cli/signals.php limit       (pcntl_alarm() ends a job that takes too long)
 *   cairo php examples/cli/signals.php wait        (the script waits for a signal on a channel)
 *
 * Each prints "ready" once its handlers are in place.
 */

declare(strict_types=1);

use Cairo\Channel;

use function Cairo\all;
use function Cairo\async;
use function Cairo\await;

pcntl_async_signals(true);

/** Where a handler runs: under the scheduler it is always inside a fiber. */
function where(): string
{
    return Fiber::getCurrent() !== null ? 'in a fiber' : 'outside a fiber';
}

/** A worker's loop: SIGTERM lets the job in hand finish, SIGUSR1 reports. */
function worker(): int
{
    $stop = false;
    $jobs = 0;
    pcntl_signal(SIGTERM, static function () use (&$stop): void {
        $stop = true;
    });
    pcntl_signal(SIGUSR1, static function () use (&$jobs): void {
        echo "jobs so far: $jobs\n";
    });
    echo "ready\n";
    while (!$stop) {
        $jobs++;
        // No job waits: a signal ends the sleep early, as it does under PHP.
        $left = sleep(30);
        if ($left > 0) {
            echo "woken with $left s left\n";
        }
    }
    echo "stopped after the job in hand\n";
    return 0;
}

/** Code that never waits is interrupted where it runs. */
function busy(): int
{
    $where = null;
    pcntl_signal(SIGUSR1, static function () use (&$where): void {
        $where = where();
    });
    echo "ready\n";
    $until = microtime(true) + 10;
    for ($n = 0; $where === null && microtime(true) < $until; $n++) {
        hash('sha256', (string) $n);
    }
    echo "handled $where\n";
    return $where === null ? 1 : 0;
}

/** $count fibers wait a second each, and a handler runs meanwhile. */
function tasks(int $count): int
{
    $handled = [];
    pcntl_signal(SIGUSR1, static function () use (&$handled): void {
        $handled[] = where();
    });
    $start = microtime(true);
    $tasks = [];
    for ($i = 0; $i < $count; $i++) {
        $tasks[] = async(static function () use (&$handled): bool {
            sleep(1);
            return $handled !== [];
        });
    }
    echo "ready\n";
    $seen = count(array_filter(await(all($tasks))));
    printf("%d waits of 1 s, %d saw the signal handled %s\n", $count, $seen, $handled[0] ?? 'nowhere');
    return microtime(true) - $start < 3 ? 0 : 1;
}

/** A job with a time limit, as a queue worker gives one: SIGALRM ends it. */
function limit(): int
{
    pcntl_signal(SIGALRM, static function (): void {
        echo "the job took too long\n";
        exit(3);
    });
    pcntl_alarm(1);
    echo "ready\n";
    $tasks = [];
    for ($i = 0; $i < 3; $i++) {
        $tasks[] = async(static fn (): int => sleep(30));
    }
    await(all($tasks));
    return 1;
}

/**
 * Waiting for a signal as a fiber waits for anything: on a channel. A
 * handler itself may not wait, as PHP lets no fiber switch inside one, so
 * it hands the signal over with trySend(), which never does.
 */
function wait(): int
{
    $signals = new Channel(1);
    pcntl_signal(SIGINT, static fn (int $signal) => $signals->trySend($signal));
    pcntl_signal(SIGTERM, static fn (int $signal) => $signals->trySend($signal));
    echo "ready\n";
    echo 'signal ', $signals->recv(), "\n";
    return 0;
}

/** What a script has of pcntl, as JSON. */
function about(): int
{
    $functions = [];
    foreach (['pcntl_signal', 'pcntl_async_signals', 'pcntl_signal_dispatch', 'pcntl_alarm', 'pcntl_exec', 'pcntl_fork', 'pcntl_sigwaitinfo'] as $function) {
        $functions[$function] = function_exists($function);
    }
    $handler = static fn () => null;
    pcntl_signal(SIGUSR2, $handler);
    $kept = pcntl_signal_get_handler(SIGUSR2) === $handler;
    pcntl_signal(SIGUSR2, SIG_DFL);
    echo json_encode(['functions' => $functions, 'kept' => $kept, 'reset' => pcntl_signal_get_handler(SIGUSR2) === SIG_DFL]), "\n";
    return 0;
}

exit(match ($argv[1] ?? '') {
    'worker' => worker(),
    'busy' => busy(),
    'tasks' => tasks((int) ($argv[2] ?? 50)),
    'limit' => limit(),
    'wait' => wait(),
    'about' => about(),
    default => 2,
});

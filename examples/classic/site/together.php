<?php

// Cairo's waits work in a classic request as they do in a fiber: while the
// script waits, the fibers it started with async() run.

declare(strict_types=1);

use Cairo\Channel;
use Cairo\Context;

use function Cairo\async;
use function Cairo\await;
use function Cairo\offload;
use function Cairo\select;

$start = microtime(true);
$ch = new Channel(1);
$task = async(static function () use ($ch, $start): void {
    Cairo\sleep(0.1);
    $ch->send((int) round((microtime(true) - $start) * 1000));
});
Cairo\sleep(0.2);
$sentAfter = $ch->recv();
[$timeout] = select(['never' => new Channel()], 0.05);
try {
    Context::current()->withTimeout(0.05)->run(static fn () => select([new Channel()]));
    $derived = 'no deadline';
} catch (Context\DeadlineExceeded) {
    $derived = 'deadline';
}
await($task);
echo json_encode([
    'sent_after_ms' => $sentAfter,
    'timeout' => $timeout,
    'derived' => $derived,
    'offload' => offload('strtoupper', 'abc'),
    'ms' => (int) round((microtime(true) - $start) * 1000),
]), "\n";

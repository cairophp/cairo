<?php

// A classic request may still wait on several things at once: each async()
// is a fiber of this request, and a wait suspends only its fiber.

$start = microtime(true);
$a = Cairo\async(static function (): string {
    usleep(500_000);
    return 'a';
});
$b = Cairo\async(static function (): string {
    usleep(500_000);
    return 'b';
});
echo Cairo\await($a), Cairo\await($b), ' ', (int) round((microtime(true) - $start) * 1000), "ms\n";

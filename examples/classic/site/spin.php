<?php

// A loop that never waits, past the deadline that CAIRO_TIMEOUT gives the
// server's requests: it is stopped where it runs, and the thread is free.

declare(strict_types=1);

$i = 0;
while (true) {
    $i++;
}

<?php

// What a classic script may do once per PHP request: declare functions and
// constants, keep globals. Each request is a fresh PHP request, so none of
// it is there for the next one.

function hits(): int
{
    global $hits;
    return $hits = ($hits ?? 0) + 1;
}

echo json_encode([
    'hits' => hits(),
    'defined' => defined('LOADED'),
    'statics' => Counter::$n++,
]), "\n";
define('LOADED', true);

class Counter
{
    public static int $n = 0;
}

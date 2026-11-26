<?php

// putenv() changes the request's own environment, as Laravel's .env loader
// does on every request: getenv() sees it in this request, and no other
// request, on this thread or another, ever does.

$before = getenv('CLASSIC_SEEN');
for ($i = 0; $i < 50; $i++) {
    putenv("CLASSIC_VAR_$i=" . str_repeat('v', 32));
}
putenv('CLASSIC_SEEN=' . ($_GET['n'] ?? '1'));
putenv('HOME');

echo json_encode([
    'before' => $before,
    'after' => getenv('CLASSIC_SEEN'),
    'home' => getenv('HOME'),
    'listed' => getenv()['CLASSIC_SEEN'] ?? null,
]), "\n";

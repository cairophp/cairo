<?php

// flush() sends what the script wrote so far: the client has it while the
// script goes on.

declare(strict_types=1);

echo "first\n";
flush();
Cairo\sleep(0.5);
echo "second\n";

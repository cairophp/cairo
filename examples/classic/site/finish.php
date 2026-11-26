<?php

// fastcgi_finish_request() answers now, and the script runs on: what it
// writes after is no one's, and the client does not wait for its end.

declare(strict_types=1);

echo "answered\n";
fastcgi_finish_request();
echo "lost\n";
usleep(800_000);

<?php

// exit() ends the script, and the functions it registered run before the
// request is answered, as under PHP-FPM.

register_shutdown_function(static function (): void {
    echo "shutdown\n";
});
echo "before\n";
exit;

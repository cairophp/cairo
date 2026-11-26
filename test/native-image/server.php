<?php

declare(strict_types=1);

// Answers whether the extension that the image added is loaded in a server's
// request thread.

use Cairo\App;
use Cairo\Http\Response;

$app = new App();
$app->http(':8080')->get('/{$}', fn (): Response => Response::text(extension_loaded('shmop') ? 'shmop' : 'none'));

return $app;

<?php
/**
 * The script of a request to Http\Server::scripts(), in classic mode: Go
 * named it, and it runs as PHP runs a request's script.
 */

declare(strict_types=1);

namespace Cairo\Internal {

    use Cairo\Context;
    use Cairo\Context\DeadlineExceeded;
    use Cairo\Http\Request;

    /** @internal */
    final class Scripts
    {
        /**
         * Runs the script of $req in this PHP request. What it echoes and the
         * headers it sets are the answer (isolated mode's classic answer).
         */
        public static function run(Request $req): void
        {
            /** @var array{file: string, name: string, path_info: string, root: string, port: string} $script */
            $script = $req->event()['script'];
            $_SERVER['SCRIPT_FILENAME'] = $script['file'];
            $_SERVER['SCRIPT_NAME'] = $script['name'];
            $_SERVER['PHP_SELF'] = $script['name'] . $script['path_info'];
            $_SERVER['DOCUMENT_ROOT'] = $script['root'];
            $_SERVER['SERVER_PORT'] = $script['port'];
            $_SERVER['SERVER_SOFTWARE'] = 'Cairo';
            $_SERVER['GATEWAY_INTERFACE'] = 'CGI/1.1';
            $_SERVER['REQUEST_SCHEME'] = ($_SERVER['HTTPS'] ?? '') === 'on' ? 'https' : 'http';
            if ($script['path_info'] !== '') {
                $_SERVER['PATH_INFO'] = $script['path_info'];
            } else {
                unset($_SERVER['PATH_INFO']);
            }
            \cairo_http_input($req->body());
            // Relative includes and file names start where the script is, as
            // under PHP-FPM.
            chdir(dirname($script['file']));
            \cairo_run_script($script['file']);
            // Past the deadline the answer is a 504, as Go's own timeout gives:
            // an uncaught DeadlineExceeded made the script's status 500, and
            // which of the two the client got depended on which came first.
            if (Context::current()->err() instanceof DeadlineExceeded) {
                throw new DeadlineExceeded();
            }
        }
    }
}

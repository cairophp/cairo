<?php
/**
 * Whether a file given to `cairo serve` declares an app, for the thread that
 * reads the declaration. Apart from plain.php because it reads $_SERVER and
 * $_ENV: a file that names them has PHP fill them as the file loads, with the
 * whole environment, and a plain script's thread loads plain.php for each
 * request.
 */

declare(strict_types=1);

namespace Cairo\Internal {

    /**
     * Whether a PHP file declares a Cairo app, read with PHP's tokenizer and
     * never run: running it would run a plain script's code at start.
     *
     * @internal
     */
    final class Declares
    {
        private const SYMFONY_RUNTIME = 'Cairo\\Symfony\\Runtime';

        public static function app(string $file): bool
        {
            $tokens = \PhpToken::tokenize((string) file_get_contents($file));
            $namespaced = false;
            $imported = false;   // `use Cairo;`: Cairo\App then means \Cairo\App in a namespace too
            $runtime = false;
            foreach ($tokens as $i => $token) {
                if ($token->is(T_NAMESPACE)) {
                    $namespaced = true;
                } elseif ($token->is(T_USE) && ($name = self::nameAfter($tokens, $i)) !== null) {
                    $name = ltrim($name, '\\');
                    if ($name === 'Cairo\\App' || ($name === 'Cairo\\' && self::groupNames($tokens, $i, 'App'))) {
                        return true;
                    }
                    $imported = $imported || $name === 'Cairo';
                } elseif ($token->is(T_NAME_FULLY_QUALIFIED) && $token->text === '\\Cairo\\App') {
                    return true;
                } elseif ($token->is(T_NAME_QUALIFIED) && $token->text === 'Cairo\\App' && (!$namespaced || $imported)) {
                    return true;
                } elseif ($token->is(T_CONSTANT_ENCAPSED_STRING) && str_ends_with(trim($token->text, '\'"'), 'autoload_runtime.php')) {
                    $runtime = true;
                }
            }
            return $runtime && self::symfonyRuntime($file);
        }

        /**
         * The name a `use` at $i imports, or null when none follows (a
         * closure's use, or a trait's inside a class, which import nothing
         * that matters here).
         *
         * @param list<\PhpToken> $tokens
         */
        private static function nameAfter(array $tokens, int $i): ?string
        {
            for ($j = $i + 1; isset($tokens[$j]); $j++) {
                $token = $tokens[$j];
                if ($token->isIgnorable()) {
                    continue;
                }
                if ($token->is([T_FUNCTION, T_CONST])) {
                    return null;
                }
                if ($token->is([T_NAME_QUALIFIED, T_NAME_FULLY_QUALIFIED, T_STRING])) {
                    $next = $tokens[$j + 1] ?? null;
                    return $next !== null && $next->is(T_NS_SEPARATOR) ? $token->text . '\\' : $token->text;
                }
                return null;
            }
            return null;
        }

        /**
         * Whether the group `use Cairo\{...}` at $i names $name.
         *
         * @param list<\PhpToken> $tokens
         */
        private static function groupNames(array $tokens, int $i, string $name): bool
        {
            for ($j = $i + 1; isset($tokens[$j]) && $tokens[$j]->text !== ';'; $j++) {
                if ($tokens[$j]->is(T_STRING) && $tokens[$j]->text === $name) {
                    return true;
                }
            }
            return false;
        }

        /** Cairo's Symfony runtime is the one chosen for $file's app. */
        private static function symfonyRuntime(string $file): bool
        {
            $chosen = $_SERVER['APP_RUNTIME'] ?? $_ENV['APP_RUNTIME'] ?? getenv('APP_RUNTIME');
            if (is_string($chosen) && $chosen !== '') {
                return ltrim($chosen, '\\') === self::SYMFONY_RUNTIME;
            }
            for ($dir = dirname($file); ; $dir = dirname($dir)) {
                if (is_file($dir . '/composer.json')) {
                    $composer = json_decode((string) file_get_contents($dir . '/composer.json'), true);
                    return ltrim((string) ($composer['extra']['runtime']['class'] ?? ''), '\\') === self::SYMFONY_RUNTIME;
                }
                if (dirname($dir) === $dir) {
                    return false;
                }
            }
        }
    }
}

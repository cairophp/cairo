<?php
/**
 * Form bodies and uploaded files of HTTP requests.
 */

declare(strict_types=1);

namespace Cairo\Internal {

    /** Form bodies and uploads: $_POST/$_FILES shapes and move_uploaded_file(). */
    final class Form
    {
        /** @var array<string, true> temporary files of requests in flight on this thread */
        private static array $uploads = [];

        public static function fields(array $event): array
        {
            $fields = [];
            if (isset($event['form'])) {
                $pairs = [];
                foreach ($event['form'] as [$name, $value]) {
                    $pairs[] = urlencode($name) . '=' . urlencode($value);
                }
                parse_str(implode('&', $pairs), $fields);
            } else {
                foreach ($event['headers'] ?? [] as $name => $values) {
                    if (strcasecmp($name, 'Content-Type') === 0
                        && str_starts_with(strtolower((string) ($values[0] ?? '')), 'application/x-www-form-urlencoded')) {
                        parse_str((string) ($event['body'] ?? ''), $fields);
                    }
                }
            }
            return $fields;
        }

        /**
         * PHP's $_FILES layout: "doc" gives $_FILES['doc']['name'], "docs[]"
         * gives $_FILES['docs']['name'][0], "a[b]" gives $_FILES['a']['name']['b'].
         */
        public static function superglobal(array $files): array
        {
            $pairs = [];
            foreach ($files as $f) {
                $field = (string) $f['field'];
                $i = strpos($field, '[');
                $base = $i === false ? $field : substr($field, 0, $i);
                $rest = $i === false ? '' : substr($field, $i);
                if ($base === '') {
                    continue;
                }
                foreach (['name', 'full_path', 'type', 'tmp_name', 'error', 'size'] as $key) {
                    $pairs[] = urlencode($base) . '[' . $key . ']' . self::encodeBrackets($rest) . '=' . urlencode((string) $f[$key]);
                }
            }
            parse_str(implode('&', $pairs), $out);
            foreach ($out as &$entry) {
                foreach (['error', 'size'] as $key) {
                    if (is_array($entry[$key] ?? null)) {
                        array_walk_recursive($entry[$key], static function (&$v): void { $v = (int) $v; });
                    } elseif (isset($entry[$key])) {
                        $entry[$key] = (int) $entry[$key];
                    }
                }
            }
            unset($entry);
            return $out;
        }

        private static function encodeBrackets(string $rest): string
        {
            return preg_replace_callback('/\[([^\]]*)\]/', static fn (array $m): string => '[' . urlencode($m[1]) . ']', $rest) ?? '';
        }

        /** @param list<array{tmp_name: string}> $files */
        public static function track(array $files): void
        {
            foreach ($files as $f) {
                if ($f['tmp_name'] !== '') {
                    self::$uploads[$f['tmp_name']] = true;
                }
            }
        }

        /** @param list<array{tmp_name: string}> $files */
        public static function untrack(array $files): void
        {
            foreach ($files as $f) {
                unset(self::$uploads[$f['tmp_name']]);
            }
        }

        public static function isUpload(string $path): bool
        {
            return isset(self::$uploads[$path]);
        }

        public static function move(string $from, string $to): bool
        {
            if (!self::isUpload($from)) {
                return false;
            }
            if (!@rename($from, $to)) {
                if (!@copy($from, $to)) {
                    return false;
                }
                @unlink($from);
            }
            @chmod($to, 0666 & ~umask());
            unset(self::$uploads[$from]);
            return true;
        }
    }
}

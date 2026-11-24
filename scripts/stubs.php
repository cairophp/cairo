<?php
/**
 * Writes stubs/cairo.php from the files under internal/core/php/ (the core)
 * and php/ (the extensions) that declare Cairo's public PHP API, with the
 * function bodies, private members and Cairo\Internal left out, for IDEs and
 * static analysers. The runtime itself is built into the cairo binary.
 *
 *   php scripts/stubs.php > stubs/cairo.php
 *
 * Needs ext-tokenizer. test/stubs_test.go fails when the file is stale.
 */

declare(strict_types=1);

// The files with public classes or functions, in the order the stubs list
// them. A new one goes here.
$sources = array_slice($argv, 1) ?: array_map(static fn (string $file): string => __DIR__ . "/../$file.php", [
    'internal/core/php/app', 'internal/core/php/context', 'internal/core/php/channel', 'internal/core/php/async',
    'internal/core/php/offload', 'php/hooks', 'php/middleware', 'php/http', 'php/http_response', 'php/http_stream', 'php/websocket', 'php/http_adapter',
    'php/grpc', 'php/grpc_client', 'php/net', 'php/cache', 'php/pool', 'internal/core/php/request_local',
]);

final class Stubs
{
    /** @var list<PhpToken> */
    private array $t;
    private int $i = 0;
    private string $out = '';

    public function __construct(string $code)
    {
        $this->t = PhpToken::tokenize($code);
    }

    public function run(): string
    {
        $n = count($this->t);
        while ($this->i < $n) {
            $tok = $this->t[$this->i];
            if ($tok->is(T_NAMESPACE)) {
                $this->namespace();
                continue;
            }
            $this->i++;  // the file header, declare(): written above
        }
        // Left-out members leave blank lines behind.
        $out = preg_replace(['/\{\n(?:[ \t]*\n)+/', '/\n(?:[ \t]*\n){2,}/'], ["{\n", "\n\n"], $this->out);
        return "\n" . trim($out) . "\n";
    }

    private function namespace(): void
    {
        $start = $this->i;
        $name = '';
        while (!$this->t[$this->i]->is('{')) {
            if ($this->t[$this->i]->is([T_NAME_QUALIFIED, T_STRING])) {
                $name = $this->t[$this->i]->text;
            }
            $this->i++;
        }
        $open = $this->i;
        $close = $this->match($open);
        if ($name === '' || $name === 'Cairo\\Internal' || str_starts_with($name, 'Cairo\\Internal\\')) {
            $this->i = $close + 1;
            return;
        }
        $this->out .= "\n" . $this->text($start, $open + 1);
        $this->i = $open + 1;
        $this->block($close, false);
        $this->out .= "}\n";
        $this->i = $close + 1;
    }

    /** Writes the members of a namespace ($inClass false) or class body up to $end. */
    private function block(int $end, bool $inClass): void
    {
        $chunk = $this->i;
        while ($this->i < $end) {
            $tok = $this->t[$this->i];
            if ($tok->is(T_FUNCTION)) {
                $body = $this->i;
                while (!$this->t[$body]->is(['{', ';'])) {
                    $body = $this->t[$body]->is('(') ? $this->match($body) + 1 : $body + 1;
                }
                $stop = $this->t[$body]->is('{') ? $this->match($body) : $body;
                $this->emit($chunk, $body, $stop, $inClass);
                $this->i = $chunk = $stop + 1;
                continue;
            }
            if ($tok->is([T_CLASS, T_INTERFACE, T_TRAIT, T_ENUM])) {
                $open = $this->i;
                while (!$this->t[$open]->is('{')) {
                    $open++;
                }
                $close = $this->match($open);
                $this->out .= $this->text($chunk, $open + 1);
                $this->i = $open + 1;
                $this->block($close, true);
                $this->out .= '}';
                $this->i = $chunk = $close + 1;
                continue;
            }
            if ($tok->is(T_USE) && !$inClass) {
                $stop = $this->i;
                while (!$this->t[$stop]->is(';')) {
                    $stop++;
                }
                $imported = trim($this->text($this->i + 1, $stop));
                if (!str_starts_with($imported, 'Cairo\\Internal')) {
                    $this->out .= $this->text($chunk, $stop + 1);
                }
                $this->i = $chunk = $stop + 1;
                continue;
            }
            if ($tok->is(';') && $inClass) {
                $this->emit($chunk, null, $this->i, true);
                $this->i = $chunk = $this->i + 1;
                continue;
            }
            if ($tok->is(['{', '[', '('])) {
                $this->i = $this->match($this->i) + 1;
                continue;
            }
            $this->i++;
        }
        $this->out .= $this->text($chunk, $end);
    }

    /**
     * Writes one declaration, tokens $from..$stop: a function whose body
     * opens at $body (replaced by {}), or a property, constant or case.
     * Private class members are left out.
     */
    private function emit(int $from, ?int $body, int $stop, bool $inClass): void
    {
        if ($inClass) {
            for ($k = $from; $k <= $stop; $k++) {
                $tok = $this->t[$k];
                if ($tok->is(T_PRIVATE)) {
                    return;
                }
                if ($tok->is([T_FUNCTION, T_VARIABLE, '='])) {
                    break;
                }
            }
        }
        if ($body !== null && $this->t[$body]->is('{')) {
            $this->out .= rtrim($this->text($from, $body)) . "\n" . $this->indent($body) . "{\n" . $this->indent($body) . '}';
            return;
        }
        $this->out .= $this->text($from, $stop + 1);
    }

    /** The whitespace before the line that holds token $k. */
    private function indent(int $k): string
    {
        $line = $this->t[$k]->line;
        while ($k > 0 && $this->t[$k - 1]->line === $line) {
            $k--;
        }
        $ws = $this->t[$k - 1] ?? null;
        if ($ws !== null && $ws->is(T_WHITESPACE)) {
            $tail = strrchr($ws->text, "\n");
            return $tail === false ? '' : substr($tail, 1);
        }
        return '';
    }

    /** The bracket that closes the one at $open. */
    private function match(int $open): int
    {
        $depth = 0;
        for ($k = $open, $n = count($this->t); $k < $n; $k++) {
            $tok = $this->t[$k];
            if ($tok->is(['{', '[', '(', T_CURLY_OPEN, T_DOLLAR_OPEN_CURLY_BRACES])) {
                $depth++;
            } elseif ($tok->is(['}', ']', ')'])) {
                if (--$depth === 0) {
                    return $k;
                }
            }
        }
        throw new RuntimeException("unbalanced bracket at line {$this->t[$open]->line}");
    }

    private function text(int $from, int $to): string
    {
        $s = '';
        for ($k = $from; $k < $to; $k++) {
            $s .= $this->t[$k]->text;
        }
        return $s;
    }
}

echo "<?php\n\n"
    . "// Generated by scripts/stubs.php from the files in php/; do not edit.\n"
    . "//\n"
    . "// Cairo's PHP API for IDEs and static analysers. The real classes are\n"
    . "// built into the cairo binary: never include or autoload this file.\n\n"
    . "declare(strict_types=1);\n";
foreach ($sources as $src) {
    $code = file_get_contents($src);
    if ($code === false) {
        fwrite(STDERR, "cannot read $src\n");
        exit(1);
    }
    echo (new Stubs($code))->run();
}

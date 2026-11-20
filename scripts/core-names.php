<?php
/**
 * Lists what the core's PHP files (internal/core/php/) name of the
 * extensions: a class, interface, trait, enum or function that a file under
 * php/ declares, or a PHP function that the extensions' C code adds
 * (internal/sapi/*.c). One line each, sorted:
 *
 *   internal/core/php/app.php: Cairo\Http\Server (php/http.php)
 *
 * The core must not name an extension. internal/core/boundary_test.go runs
 * this and fails on any line but the ones it allows. Needs ext-tokenizer.
 *
 *   php scripts/core-names.php
 */

declare(strict_types=1);

const NAMES = [T_STRING, T_NAME_QUALIFIED, T_NAME_FULLY_QUALIFIED, T_NAME_RELATIVE];

/** @return list<PhpToken> the tokens of $file without whitespace and comments */
function code(string $file): array
{
    return array_values(array_filter(PhpToken::tokenize((string) file_get_contents($file)), static fn (PhpToken $t): bool => !$t->isIgnorable()));
}

/**
 * What $file declares outside its classes: full names, by their lower case
 * (PHP does not tell Foo from foo).
 *
 * @return array<string, string>
 */
function declared(string $file): array
{
    $names = [];
    $namespace = '';
    $depth = 0;
    $classEnds = -1;   // the brace depth that the class being read closes at; -1 outside a class
    $t = code($file);
    foreach ($t as $i => $token) {
        if ($token->is(T_NAMESPACE) && $t[$i + 1]->is(NAMES)) {
            $namespace = $t[$i + 1]->text . '\\';
        } elseif ($token->is(['{', T_CURLY_OPEN, T_DOLLAR_OPEN_CURLY_BRACES])) {
            $depth++;
        } elseif ($token->is('}')) {
            if (--$depth === $classEnds) {
                $classEnds = -1;
            }
        } elseif ($classEnds < 0 && $token->is([T_CLASS, T_INTERFACE, T_TRAIT, T_ENUM, T_FUNCTION]) && $t[$i + 1]->is(T_STRING) && !$t[$i - 1]->is(T_DOUBLE_COLON)) {
            $names[strtolower($namespace . $t[$i + 1]->text)] = $namespace . $t[$i + 1]->text;
            if (!$token->is(T_FUNCTION)) {
                $classEnds = $depth;
            }
        }
    }
    return $names;
}

/**
 * Every full name that a name used in $file can stand for, in lower case:
 * the name as written, the name under the file's namespace, and the name
 * through a use statement. More than PHP would resolve, never less.
 *
 * @return array<string, true>
 */
function named(string $file): array
{
    $names = [];
    $namespace = '';
    $imports = [];
    $t = code($file);
    foreach ($t as $i => $token) {
        if ($token->is(T_NAMESPACE) && $t[$i + 1]->is(NAMES)) {
            $namespace = $t[$i + 1]->text . '\\';
        }
        if (!$token->is(NAMES) || $t[$i - 1]->is([T_OBJECT_OPERATOR, T_NULLSAFE_OBJECT_OPERATOR, T_DOUBLE_COLON, T_NAMESPACE, T_AS])) {
            continue;   // not a name, or a member, the namespace itself, an alias
        }
        $import = $t[$i - 1]->is(T_USE) || ($t[$i - 1]->is([T_FUNCTION, T_CONST]) && $t[$i - 2]->is(T_USE));
        if (!$import && $t[$i - 1]->is([T_CLASS, T_INTERFACE, T_TRAIT, T_ENUM, T_FUNCTION, T_CONST])) {
            continue;   // what the file declares itself
        }
        $name = $token->is(T_NAME_RELATIVE) ? $namespace . substr($token->text, strlen('namespace\\')) : ltrim($token->text, '\\');
        $parts = explode('\\', $name);
        if ($import) {
            $imports[strtolower($t[$i + 1]->is(T_AS) ? $t[$i + 2]->text : end($parts))] = $name;
        }
        $names[strtolower($name)] = true;
        $names[strtolower($namespace . $name)] = true;
        if (isset($imports[strtolower($parts[0])])) {
            $names[strtolower(implode('\\', [$imports[strtolower($parts[0])], ...array_slice($parts, 1)]))] = true;
        }
    }
    return $names;
}

$root = dirname(__DIR__);
$extensions = [];   // lower-case name => [the name, the file that declares it]
foreach (glob("$root/php/*.php") ?: [] as $file) {
    foreach (declared($file) as $lower => $name) {
        $extensions[$lower] = [$name, 'php/' . basename($file)];
    }
}
foreach (glob("$root/internal/sapi/*.c") ?: [] as $file) {
    preg_match_all('/\bPHP_FE\((\w+)/', (string) file_get_contents($file), $m);
    foreach ($m[1] as $name) {
        $extensions[strtolower($name)] = [$name, 'internal/sapi/' . basename($file)];
    }
}

$lines = [];
foreach (glob("$root/internal/core/php/*.php") ?: [] as $file) {
    foreach (array_intersect_key($extensions, named($file)) as [$name, $where]) {
        $lines[] = 'internal/core/php/' . basename($file) . ": $name ($where)\n";
    }
}
sort($lines);
echo implode('', $lines);

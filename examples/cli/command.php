#!/usr/bin/env php
<?php
/**
 * A command for PHP's command line in the Cairo binary: no PHP is installed
 * next to it. It is what `php artisan` and `bin/console` are in an app.
 *
 *   cairo php examples/cli/command.php about one --two
 *   echo some text | cairo php examples/cli/command.php upper
 *   CAIRO_TEST_PGSQL='pgsql:host=127.0.0.1;dbname=cairo' CAIRO_TEST_PGSQL_USER=cairo \
 *     CAIRO_TEST_PGSQL_PASSWORD=cairo cairo php examples/cli/command.php migrate pgsql
 *   cairo php examples/cli/command.php together 50          (50 waits of a second, in a second)
 *   cairo php examples/cli/command.php together 20 pgsql    (20 queries of a second, in a second)
 *
 * The script runs under Cairo's scheduler, as a request handler does: what
 * it starts with async() runs beside it, and a wait suspends only its fiber.
 *
 * With a link named php to the binary (the official image has one), the
 * first line of this file runs it too: ./examples/cli/command.php about
 */

declare(strict_types=1);

use function Cairo\all;
use function Cairo\async;
use function Cairo\await;

/** What the script was started with, as JSON. */
function about(): int
{
    echo json_encode([
        'sapi' => PHP_SAPI,
        'binary' => PHP_BINARY,
        'argv' => $_SERVER['argv'],
        'script' => $_SERVER['SCRIPT_FILENAME'],
        'included' => get_included_files(),
        'cairo' => class_exists(Cairo\App::class),
        'errors' => ini_get('display_errors'),
        'memory' => ini_get('memory_limit'),
    ], JSON_THROW_ON_ERROR | JSON_UNESCAPED_SLASHES), "\n";
    return 0;
}

/** A connection to the database that the tests name for $driver. */
function connect(string $driver): PDO
{
    $env = 'CAIRO_TEST_' . strtoupper($driver);
    return new PDO((string) getenv($env), (string) getenv($env . '_USER'), (string) getenv($env . '_PASSWORD'), [
        PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION,
    ]);
}

/** Creates a table, fills it in a transaction and reads it back. */
function migrate(string $driver): int
{
    $db = connect($driver);
    if ($driver === 'mysql') {
        // The DSN may name no database: a migration makes its own.
        $db->exec('CREATE DATABASE IF NOT EXISTS cairo_cli');
        $db->exec('USE cairo_cli');
    }
    $db->exec('DROP TABLE IF EXISTS cli_releases');
    $db->exec('CREATE TABLE cli_releases (version VARCHAR(20) PRIMARY KEY, notes TEXT)');
    $db->beginTransaction();
    $insert = $db->prepare('INSERT INTO cli_releases (version, notes) VALUES (?, ?)');
    foreach (['0.5.0' => 'the trim', '0.6.0' => 'the command line'] as $version => $notes) {
        $insert->execute([$version, $notes]);
    }
    $db->commit();
    // A fiber of the script's own, which Cairo's scheduler did not start:
    // its queries wait as they do under PHP.
    $count = new Fiber(static fn (): mixed => $db->query('SELECT COUNT(*) FROM cli_releases')->fetchColumn());
    $count->start();
    echo 'migrated ', $count->getReturn(), ' releases with ', $db->getAttribute(PDO::ATTR_DRIVER_NAME), "\n";
    $db->exec('DROP TABLE cli_releases');
    return 0;
}

/**
 * Waits a second $n times at once, each time in a fiber of its own: in PHP's
 * sleep(), or with $driver in a query on a connection of the fiber's. The
 * lot takes about a second.
 */
function together(int $n, ?string $driver): int
{
    $sql = ['pgsql' => 'SELECT pg_sleep(1), CAST(? AS INTEGER)', 'mysql' => 'SELECT SLEEP(1), ?'];
    $start = microtime(true);
    $tasks = [];
    for ($i = 0; $i < $n; $i++) {
        $tasks[] = async(static function () use ($i, $driver, $sql): int {
            if ($driver === null) {
                sleep(1);
                return $i;
            }
            $query = connect($driver)->prepare($sql[$driver]);
            $query->execute([$i]);
            return (int) $query->fetch(PDO::FETCH_NUM)[1];
        });
    }
    $sum = array_sum(await(all($tasks)));
    printf("%d waits of 1 s, sum %d, in %.1f s\n", $n, $sum, microtime(true) - $start);
    return 0;
}

/** Runs PHP again, the way a framework starts its own commands. */
function again(): int
{
    passthru(escapeshellarg(PHP_BINARY) . ' -d memory_limit=64M ' . escapeshellarg(__FILE__) . ' about again', $status);
    return $status;
}

$command = $argv[1] ?? 'about';
switch ($command) {
    case 'about':
        exit(about());
    case 'upper':
        fwrite(STDOUT, strtoupper((string) stream_get_contents(STDIN)));
        exit(0);
    case 'fail':
        fwrite(STDERR, "the command failed\n");
        exit((int) ($argv[2] ?? 1));
    case 'migrate':
        exit(migrate($argv[2] ?? 'pgsql'));
    case 'together':
        exit(together((int) ($argv[2] ?? 50), $argv[3] ?? null));
    case 'again':
        exit(again());
    default:
        fwrite(STDERR, "unknown command $command\n");
        exit(2);
}

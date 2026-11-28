package test

import (
	"strings"
	"testing"
)

// The threads of the start all read the code before the server listens,
// even when each takes a while to boot: a change made as soon as it
// answers is one that --watch serves with a restart, and no thread of the
// first process fails on it. The routes are in a file of their own here, read
// after the boot, so that a thread which still boots reads the new one.
func TestWatchSeesAChangeMadeAsTheServerStarts(t *testing.T) {
	dir := t.TempDir()
	copyReload(t, dir)
	app := &reloadApp{t: t, dir: dir}
	app.write("server.php", `<?php

declare(strict_types=1);

use Cairo\App;

$app = new App();
$http = $app->http(getenv('CAIRO_HTTP') ?: ':8080');
usleep(300_000);
require __DIR__ . '/routes.php';

return $app;
`)
	app.write("routes.php", "<?php\n\n$http->get('/{$}', fn (): Cairo\\Http\\Response => Cairo\\Http\\Response::text('hello'));\n")
	app = serveReload(t, dir, nil, "--threads", "2", "--watch")
	app.write("routes.php", "<?php\n\n$http->get('/added', fn (): Cairo\\Http\\Response => Cairo\\Http\\Response::text('added'));\n")
	app.answers("/added", "added")
	app.logged("restarting in place", 1)
	noDeclarationError(t, app)
}

// A thread that starts after server.php changed on disk (here one that a
// fatal error ended, without --watch: a deploy that has not reloaded yet)
// cannot serve the routes the servers were made with. It ends quietly and
// the process starts anew in place, with the new declaration.
func TestThreadThatFindsANewDeclarationRestartsTheProcess(t *testing.T) {
	app := startReload(t, "--threads", "1")
	app.answers("/", "hello")
	app.declare("/added", "added")
	if code, _ := app.get("/crash"); code != 500 {
		t.Fatalf("/crash: %d, want 500", code)
	}
	app.answers("/added", "added")
	app.logged("restarting in place", 1)
	noDeclarationError(t, app)
}

func noDeclarationError(t *testing.T, app *reloadApp) {
	t.Helper()
	if log := app.logs.String(); strings.Contains(log, "different server declaration") {
		t.Errorf("a thread failed on the new declaration:\n%s", log)
	}
}

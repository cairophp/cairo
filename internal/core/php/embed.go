// Package php holds the PHP side of the core engine, embedded in the binary.
package php

import "embed"

// Files are the core's PHP files. Every PHP thread runs runtime.php, which
// loads the others (request_local.php on first use of its class) and then
// the files that the extensions added with core.RegisterPHP.
//
//go:embed *.php
var Files embed.FS

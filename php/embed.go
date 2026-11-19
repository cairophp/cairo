// Package php holds the PHP side of the Cairo runtime, embedded in the binary.
package php

import "embed"

// Files are the PHP files of the runtime. Every PHP thread runs runtime.php,
// which loads the others: most of them at start, the rest on first use of a
// class they declare.
//
//go:embed *.php
var Files embed.FS

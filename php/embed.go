// Package php holds the PHP side of Cairo's extensions, embedded in the
// binary. The core's own PHP files are in internal/core/php.
package php

import "embed"

// Files are the PHP files of the extensions. Each one joins the runtime
// with core.RegisterPHP: most are loaded by every PHP thread at start, the
// rest on first use of a class they declare.
//
//go:embed *.php
var Files embed.FS

// Package php holds the PHP side of the Cairo runtime, embedded in the binary.
package php

import _ "embed"

// Runtime is php/runtime.php: the Cairo\ classes, the fiber scheduler and
// the entry point every PHP thread runs.
//
//go:embed runtime.php
var Runtime []byte

// Revolt is php/revolt.php: Cairo\Revolt\Driver, loaded on first use when
// revolt/event-loop is installed.
//
//go:embed revolt.php
var Revolt []byte

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

// Cache is php/cache.php: Cairo\Cache, loaded on first use.
//
//go:embed cache.php
var Cache []byte

// Pool is php/pool.php: Cairo\Pool, loaded on first use.
//
//go:embed pool.php
var Pool []byte

// Queries is php/queries.php: the repeated query detector, loaded on first use.
//
//go:embed queries.php
var Queries []byte

package sapi

// The extensions with no Go code in this package: the middleware a server
// declares (its Go half is internal/middleware), Cairo\Pool and the driver
// for Revolt.
func init() {
	registerPHP("middleware.php")
	registerPHP("pool.php", `Cairo\Pool`, `Cairo\PoolTimeout`)
	registerPHP("revolt.php", `Cairo\Revolt\Driver`, `Cairo\Revolt\Suspension`, `Cairo\Revolt\Callback`, `Cairo\Revolt\Watch`)
}

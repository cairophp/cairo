package sapi

// The extensions with no Go code in this package: the middleware a server
// declares (its Go half is internal/middleware) and Cairo\Pool.
func init() {
	registerPHP("middleware.php")
	registerPHP("pool.php", `Cairo\Pool`, `Cairo\PoolTimeout`)
}

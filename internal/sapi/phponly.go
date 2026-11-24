package sapi

// The extensions with no Go code in this package: the middleware a server
// declares (its Go half is internal/middleware), the Mercure hub a server
// declares (internal/mercurehub) and Cairo\Pool.
func init() {
	registerPHP("middleware.php")
	registerPHP("mercure.php", `Cairo\Http\Mercure`)
	registerPHP("pool.php", `Cairo\Pool`, `Cairo\PoolTimeout`)
}

package sapi

// The extensions with no Go code in this package: the PHP half of the HTTP
// server (its Go half is internal/httpserver), the middleware a server
// declares, Cairo\Pool and the driver for Revolt.
func init() {
	registerPHP("middleware.php")
	registerPHP("http.php")
	registerPHP("form.php")
	registerPHP("superglobals.php")
	registerPHP("http_adapter.php")
	registerPHP("pool.php", `Cairo\Pool`, `Cairo\PoolTimeout`)
	registerPHP("revolt.php", `Cairo\Revolt\Driver`, `Cairo\Revolt\Suspension`, `Cairo\Revolt\Callback`, `Cairo\Revolt\Watch`)
}

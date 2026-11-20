package sapi

// What the extensions run when the engine starts: server.php has been
// declared and the PHP threads are up, and no request has been taken yet.
var startFuncs []func(e *Engine) error

// registerStart adds a function that Start runs; call it from init(). It is
// where an extension reads its own part of the declaration (Spec.Section)
// and turns itself on. Its error fails Start.
func registerStart(fn func(e *Engine) error) {
	startFuncs = append(startFuncs, fn)
}

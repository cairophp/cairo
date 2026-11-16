package middleware

import "context"

// Values that Go middleware hands to PHP, read there with
// Cairo\Context::value($key).

type valuesKey struct{}

// WithValue returns a copy of ctx in which key is set to v for PHP.
func WithValue(ctx context.Context, key string, v any) context.Context {
	old := Values(ctx)
	next := make(map[string]any, len(old)+1)
	for k, x := range old {
		next[k] = x
	}
	next[key] = v
	return context.WithValue(ctx, valuesKey{}, next)
}

// Values returns the values set with WithValue. Do not modify the map.
func Values(ctx context.Context) map[string]any {
	m, _ := ctx.Value(valuesKey{}).(map[string]any)
	return m
}

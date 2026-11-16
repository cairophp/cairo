package middleware

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Panic recovery is always on, outside every declared middleware: a bug in
// Cairo's Go code (or a Go middleware) while serving one request answers that
// request with an error and logs the stack, instead of taking the process
// down (gRPC) or dropping the connection silently (HTTP).

func recoverHTTP(h http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler {
				panic(v) // net/http's way to abort a response on purpose
			}
			log.Error("panic serving http request", "method", r.Method, "path", r.URL.Path, "panic", fmt.Sprint(v), "stack", string(debug.Stack()))
			// If the handler already wrote headers this only appends; the
			// client still sees a broken response rather than a hang.
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}()
		h.ServeHTTP(w, r)
	})
}

func recoverGRPC(log *slog.Logger) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		defer func() {
			if v := recover(); v != nil {
				log.Error("panic serving grpc call", "method", info.FullMethod, "panic", fmt.Sprint(v), "stack", string(debug.Stack()))
				err = status.Error(codes.Internal, "internal error")
			}
		}()
		return handler(srv, ss)
	}
}

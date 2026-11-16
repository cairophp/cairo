package middleware

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/mohamed-samir907/cairo/internal/spec"
)

// GRPC returns the stream interceptors for the declared middleware, first
// declared outermost. Cairo serves every call through a stream handler, so
// stream interceptors cover unary calls too.
func GRPC(decl []spec.Middleware, log *slog.Logger) ([]grpc.StreamServerInterceptor, error) {
	out := []grpc.StreamServerInterceptor{recoverGRPC(log)}
	for _, m := range decl {
		switch m.Name {
		case "request_id":
			out = append(out, grpcRequestID)
		case "access_log":
			out = append(out, grpcAccessLog(log))
		case "rate_limit":
			var o RateOptions
			if err := decode(m, &o); err != nil {
				return nil, fmt.Errorf("middleware rate_limit: %w", err)
			}
			l, err := newLimiter(o)
			if err != nil {
				return nil, fmt.Errorf("middleware rate_limit: %w", err)
			}
			out = append(out, grpcRateLimit(l))
		case "timeout":
			d, err := timeoutOf(m)
			if err != nil {
				return nil, fmt.Errorf("middleware timeout: %w", err)
			}
			out = append(out, grpcTimeout(d))
		default:
			return nil, fmt.Errorf("middleware %q is not available for gRPC servers", m.Name)
		}
	}
	return out, nil
}

type ctxStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s ctxStream) Context() context.Context { return s.ctx }

const grpcRequestIDKey = "x-request-id"

// RequestID returns the call's request id set by the request_id middleware.
func RequestID(ctx context.Context) string {
	md, _ := metadata.FromIncomingContext(ctx)
	if v := md.Get(grpcRequestIDKey); len(v) > 0 {
		return v[0]
	}
	return ""
}

func grpcRequestID(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	ctx := ss.Context()
	id := RequestID(ctx)
	if !validID(id) {
		id = NewRequestID()
		md, _ := metadata.FromIncomingContext(ctx)
		md = md.Copy()
		md.Set(grpcRequestIDKey, id)
		ctx = metadata.NewIncomingContext(ctx, md)
	}
	_ = ss.SetHeader(metadata.Pairs(grpcRequestIDKey, id))
	return handler(srv, ctxStream{ss, ctx})
}

func grpcAccessLog(log *slog.Logger) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		start := time.Now()
		err := handler(srv, ss)
		remote := ""
		if p, ok := peer.FromContext(ss.Context()); ok {
			remote = p.Addr.String()
		}
		log.Info("grpc", "method", info.FullMethod, "code", status.Code(err).String(),
			"took", time.Since(start).Round(time.Microsecond), "remote", remote, "request_id", RequestID(ss.Context()))
		return err
	}
}

func grpcRateLimit(l *limiter) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		key := ""
		if p, ok := peer.FromContext(ss.Context()); ok {
			key = clientIP(p.Addr.String())
		}
		if ok, wait := l.allow(key); !ok {
			return status.Errorf(codes.ResourceExhausted, "rate limit exceeded; retry in %s", wait.Round(time.Millisecond))
		}
		return handler(srv, ss)
	}
}

// grpcTimeout gives each call a deadline, or keeps the client's when sooner.
func grpcTimeout(d time.Duration) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx, cancel := context.WithTimeout(ss.Context(), d)
		defer cancel()
		return handler(srv, ctxStream{ss, ctx})
	}
}

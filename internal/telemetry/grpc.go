package telemetry

import (
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"google.golang.org/grpc"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
)

// GRPCServer is the options of a gRPC server whose calls count in the
// metrics and become spans. Only the methods that known reports true for
// do: any client can call a method by any name, and a name must not become
// a label or a span.
func (t *Telemetry) GRPCServer(known func(fullMethod string) bool) []grpc.ServerOption {
	if t == nil {
		return nil
	}
	var opts []grpc.ServerOption
	if t.metrics != nil {
		opts = append(opts, grpc.ChainStreamInterceptor(t.metrics.measureGRPC(known)))
	}
	if t.tracer != nil {
		opts = append(opts, grpc.StatsHandler(t.statsHandler(known)))
	}
	return opts
}

// statsHandler makes the spans of a gRPC server's calls.
func (t *Telemetry) statsHandler(known func(fullMethod string) bool) stats.Handler {
	return otelgrpc.NewServerHandler(
		otelgrpc.WithTracerProvider(t.tracer),
		otelgrpc.WithMeterProvider(metricnoop.NewMeterProvider()),
		otelgrpc.WithPropagators(propagator),
		otelgrpc.WithFilter(func(info *stats.RPCTagInfo) bool { return known(info.FullMethodName) }),
	)
}

// measureGRPC counts each call of a known method and how long it took.
func (m *metrics) measureGRPC(known func(fullMethod string) bool) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if !known(info.FullMethod) {
			return handler(srv, ss)
		}
		start := time.Now()
		err := handler(srv, ss)
		m.grpc.WithLabelValues(strings.TrimPrefix(info.FullMethod, "/"), status.Code(err).String()).Observe(time.Since(start).Seconds())
		return err
	}
}

// GRPCClient is the options of a gRPC client that PHP opens while tracing
// is on: each call is a span under the request that makes it, and carries
// the trace to the server it calls.
func GRPCClient() []grpc.DialOption {
	t := current.Load()
	if t == nil || t.tracer == nil {
		return nil
	}
	return []grpc.DialOption{grpc.WithStatsHandler(otelgrpc.NewClientHandler(
		otelgrpc.WithTracerProvider(t.tracer),
		otelgrpc.WithMeterProvider(metricnoop.NewMeterProvider()),
		otelgrpc.WithPropagators(propagator),
	))}
}

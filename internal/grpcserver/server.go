// Package grpcserver serves the gRPC servers declared in server.php.
//
// Go compiles the declared .proto files at startup, so neither protoc nor
// generated Go code is needed. For each call, Go converts the request with
// the method's descriptor into the array protojson has for it, hands PHP that
// array, and converts PHP's reply the same way (message.go). Health checking
// and server reflection are built in.
package grpcserver

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/bufbuild/protocompile"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
	reflectionv1 "google.golang.org/grpc/reflection/grpc_reflection_v1"
	reflectionv1alpha "google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/cairophp/cairo/internal/middleware"
	"github.com/cairophp/cairo/internal/telemetry"
	"github.com/cairophp/cairo/internal/wire"
)

// Dispatcher sends a call to PHP and returns PHP's answer: its status in
// meta, and its reply or the status's text in body.
type Dispatcher interface {
	Dispatch(ctx context.Context, ev wire.Event) (meta, body []byte, err error)
}

type method struct {
	serviceID int
	name      string
	desc      protoreflect.MethodDescriptor
	in, out   Message
}

// Server is one gRPC listener.
type Server struct {
	grpc        *grpc.Server
	ln          net.Listener
	methods     map[string]method // "/pkg.Service/Method" -> method
	services    map[string]grpc.ServiceInfo
	files       *protoregistry.Files
	maxDuration time.Duration
	php         Dispatcher
	log         *slog.Logger
}

// New compiles the declared protos, registers the services and opens the listener.
// With tel, the calls of its services are traced and measured, as declared.
func New(decl Declaration, php Dispatcher, log *slog.Logger, tel *telemetry.Telemetry) (*Server, error) {
	files, err := compileProtos(decl.Protos)
	if err != nil {
		return nil, err
	}
	s := &Server{
		methods:     map[string]method{},
		services:    map[string]grpc.ServiceInfo{},
		files:       files,
		maxDuration: time.Duration(decl.MaxDuration * float64(time.Second)),
		php:         php,
		log:         log,
	}
	hs := health.NewServer()
	for _, sv := range decl.Services {
		d, err := files.FindDescriptorByName(protoreflect.FullName(sv.Name))
		if err != nil {
			return nil, fmt.Errorf("gRPC service %q is not defined in the declared .proto files", sv.Name)
		}
		sd, ok := d.(protoreflect.ServiceDescriptor)
		if !ok {
			return nil, fmt.Errorf("%q is not a gRPC service", sv.Name)
		}
		info := grpc.ServiceInfo{Metadata: sd.ParentFile().Path()}
		ms := sd.Methods()
		for i := 0; i < ms.Len(); i++ {
			md := ms.Get(i)
			s.methods["/"+sv.Name+"/"+string(md.Name())] = method{
				serviceID: sv.ID, name: string(md.Name()), desc: md,
				in: NewMessage(md.Input()), out: NewMessage(md.Output()),
			}
			info.Methods = append(info.Methods, grpc.MethodInfo{Name: string(md.Name())})
		}
		s.services[sv.Name] = info
		hs.SetServingStatus(sv.Name, healthpb.HealthCheckResponse_SERVING)
	}

	interceptors, err := middleware.GRPC(decl.Middleware, log)
	if err != nil {
		return nil, err
	}
	opts := []grpc.ServerOption{grpc.UnknownServiceHandler(s.handle), grpc.ChainStreamInterceptor(interceptors...),
		grpc.ForceServerCodecV2(NewCodec())}
	s.grpc = grpc.NewServer(append(opts, tel.GRPCServer(s.serves)...)...)
	healthpb.RegisterHealthServer(s.grpc, hs)
	ropts := reflection.ServerOptions{Services: s, DescriptorResolver: resolver{files}}
	reflectionv1.RegisterServerReflectionServer(s.grpc, reflection.NewServerV1(ropts))
	reflectionv1alpha.RegisterServerReflectionServer(s.grpc, reflection.NewServer(ropts))

	s.ln, err = net.Listen("tcp", decl.Listen)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// serves reports whether full names a method of a declared service: those
// are the calls that telemetry follows.
func (s *Server) serves(full string) bool {
	_, ok := s.methods[full]
	return ok
}

// Addr is the address the server listens on.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Serve blocks until the server stops.
func (s *Server) Serve() error {
	err := s.grpc.Serve(s.ln)
	if errors.Is(err, grpc.ErrServerStopped) {
		return nil
	}
	return err
}

// Shutdown stops accepting calls and waits for in-flight ones, up to ctx.
func (s *Server) Shutdown(ctx context.Context) error {
	done := make(chan struct{})
	go func() { s.grpc.GracefulStop(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		s.grpc.Stop()
		return ctx.Err()
	}
}

// GetServiceInfo lists the PHP services plus the built-in ones, for reflection.
func (s *Server) GetServiceInfo() map[string]grpc.ServiceInfo {
	out := s.grpc.GetServiceInfo()
	for k, v := range s.services {
		out[k] = v
	}
	return out
}

// answer reads what cairo_respond_grpc() (internal/sapi/grpc_answer.c) puts
// before PHP's answer: whether the call succeeded, and its status code.
func answer(meta []byte) (ok bool, code codes.Code, err error) {
	if len(meta) != 5 {
		return false, 0, fmt.Errorf("an answer of %d bytes", len(meta))
	}
	return meta[0] != 0, codes.Code(binary.LittleEndian.Uint32(meta[1:])), nil
}

// event is a call as PHP receives it.
type event struct {
	full            string // /pkg.Service/Method, for logs
	Service         int
	Method          string
	Values          map[string]any
	Message         wire.Raw // the request as PHP's array
	Stream          int64
	ClientStreaming bool
	ServerStreaming bool
}

// RouteName is the full gRPC method, for the slow request log.
func (ev *event) RouteName() string { return ev.full }

func (ev *event) AppendWire(b []byte, id int64, deadline float64) []byte {
	n := 4
	for _, set := range []bool{deadline > 0, len(ev.Values) > 0, ev.Message != nil, ev.Stream != 0} {
		if set {
			n++
		}
	}
	if ev.Stream != 0 {
		n += 2
	}
	b = wire.AppendMap(b, n)
	b = wire.AppendString(wire.AppendKey(b, "type"), "grpc")
	b = wire.AppendInt(wire.AppendKey(b, "id"), id)
	if deadline > 0 {
		b = wire.AppendFloat(wire.AppendKey(b, "deadline"), deadline)
	}
	b = wire.AppendInt(wire.AppendKey(b, "service"), int64(ev.Service))
	b = wire.AppendString(wire.AppendKey(b, "method"), ev.Method)
	if len(ev.Values) > 0 {
		b = wire.AppendValue(wire.AppendKey(b, "values"), ev.Values)
	}
	if ev.Message != nil {
		b = append(wire.AppendKey(b, "message"), ev.Message...)
	}
	if ev.Stream != 0 {
		b = wire.AppendInt(wire.AppendKey(b, "stream"), ev.Stream)
		b = wire.AppendBool(wire.AppendKey(b, "client_streaming"), ev.ClientStreaming)
		b = wire.AppendBool(wire.AppendKey(b, "server_streaming"), ev.ServerStreaming)
	}
	return b
}

func (s *Server) handle(_ any, stream grpc.ServerStream) error {
	full, ok := grpc.MethodFromServerStream(stream)
	if !ok {
		return status.Error(codes.Internal, "no method in stream")
	}
	m, ok := s.methods[full]
	if !ok {
		return status.Errorf(codes.Unimplemented, "unknown method %s", full)
	}
	ctx := stream.Context()
	if tp := telemetry.Traceparent(ctx); tp != "" {
		ctx = middleware.WithValue(ctx, "traceparent", tp) // for PHP to continue the trace
	}
	ev := &event{
		full:    full,
		Service: m.serviceID,
		Method:  m.name,
		Values:  middleware.Values(ctx),
	}
	// A client-streaming method's messages are read by PHP through its
	// stream; every other method starts with one request message.
	if !m.desc.IsStreamingClient() {
		var in Frame
		if err := stream.RecvMsg(&in); err != nil {
			return err
		}
		var err error
		if ev.Message, err = m.in.ToPHP(make([]byte, 0, 2*len(in.Data)+64), in.Data); err != nil {
			return status.Errorf(codes.Internal, "grpc: failed to unmarshal the received message: %v", err)
		}
	}
	if m.desc.IsStreamingClient() || m.desc.IsStreamingServer() {
		id := openStream(stream, m)
		defer closeStream(id)
		ev.Stream = id
		ev.ClientStreaming = m.desc.IsStreamingClient()
		ev.ServerStreaming = m.desc.IsStreamingServer()
	}

	if s.maxDuration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.maxDuration)
		defer cancel()
	}
	meta, body, err := s.php.Dispatch(ctx, ev)
	if err != nil {
		if ctx.Err() == nil {
			// The PHP thread died with the call (a fatal error).
			return status.Error(codes.Internal, err.Error())
		}
		return status.FromContextError(err).Err()
	}
	ok, code, err := answer(meta)
	if err != nil {
		return status.Errorf(codes.Internal, "decode php response: %v", err)
	}
	if !ok {
		return status.Error(code, string(body))
	}
	if m.desc.IsStreamingServer() {
		return nil // PHP sent the replies through the stream
	}
	out, err := m.out.FromPHP(body)
	if err != nil {
		s.log.Error("php returned a message that does not match the proto", "method", full, "err", err)
		return status.Errorf(codes.Internal, "handler returned an invalid %s: %v", m.desc.Output().FullName(), err)
	}
	return stream.SendMsg(&Frame{out})
}

// CompileProtos compiles .proto files (and their imports) into a registry.
func CompileProtos(paths []string) (*protoregistry.Files, error) {
	return compileProtos(paths)
}

func compileProtos(paths []string) (*protoregistry.Files, error) {
	files := new(protoregistry.Files)
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		c := protocompile.Compiler{
			Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{
				ImportPaths: []string{filepath.Dir(abs)},
			}),
		}
		compiled, err := c.Compile(context.Background(), filepath.Base(abs))
		if err != nil {
			return nil, fmt.Errorf("compile %s: %w", p, err)
		}
		for _, f := range compiled {
			if err := registerWithDeps(files, f); err != nil {
				return nil, err
			}
		}
	}
	return files, nil
}

func registerWithDeps(files *protoregistry.Files, f protoreflect.FileDescriptor) error {
	if _, err := files.FindFileByPath(f.Path()); err == nil {
		return nil
	}
	imports := f.Imports()
	for i := 0; i < imports.Len(); i++ {
		if err := registerWithDeps(files, imports.Get(i).FileDescriptor); err != nil {
			return err
		}
	}
	// The compiler's descriptors answer each question (a field's kind, a
	// file's syntax) through a wrapper, which dynamicpb and protojson ask
	// for every field of every message; protobuf-go's own are built once.
	native, err := protodesc.NewFile(protodesc.ToFileDescriptorProto(f), files)
	if err != nil {
		return fmt.Errorf("describe %s: %w", f.Path(), err)
	}
	if err := files.RegisterFile(native); err != nil && !strings.Contains(err.Error(), "already registered") {
		return fmt.Errorf("register %s: %w", f.Path(), err)
	}
	return nil
}

// resolver looks in the declared protos first, then in the files linked into
// the binary (health, reflection), so reflection can describe both.
type resolver struct{ files *protoregistry.Files }

func (r resolver) FindFileByPath(path string) (protoreflect.FileDescriptor, error) {
	if f, err := r.files.FindFileByPath(path); err == nil {
		return f, nil
	}
	return protoregistry.GlobalFiles.FindFileByPath(path)
}

func (r resolver) FindDescriptorByName(name protoreflect.FullName) (protoreflect.Descriptor, error) {
	if d, err := r.files.FindDescriptorByName(name); err == nil {
		return d, nil
	}
	return protoregistry.GlobalFiles.FindDescriptorByName(name)
}

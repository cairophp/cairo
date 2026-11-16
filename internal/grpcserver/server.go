// Package grpcserver serves the gRPC servers declared in server.php.
//
// Go compiles the declared .proto files at startup, so neither protoc nor
// generated Go code is needed. For each call, Go decodes the request with the
// method's descriptor, hands PHP the message as JSON (protojson), and encodes
// PHP's reply the same way. Health checking and server reflection are built in.
package grpcserver

import (
	"context"
	"encoding/json"
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
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/mohamed-samir907/cairo/internal/middleware"
	"github.com/mohamed-samir907/cairo/internal/spec"
)

// Dispatcher sends a call to PHP and returns PHP's JSON response.
type Dispatcher interface {
	Dispatch(ctx context.Context, ev map[string]any) ([]byte, error)
}

type method struct {
	serviceID int
	name      string
	desc      protoreflect.MethodDescriptor
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
func New(decl spec.GRPCServer, php Dispatcher, log *slog.Logger) (*Server, error) {
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
			s.methods["/"+sv.Name+"/"+string(md.Name())] = method{serviceID: sv.ID, name: string(md.Name()), desc: md}
			info.Methods = append(info.Methods, grpc.MethodInfo{Name: string(md.Name())})
		}
		s.services[sv.Name] = info
		hs.SetServingStatus(sv.Name, healthpb.HealthCheckResponse_SERVING)
	}

	interceptors, err := middleware.GRPC(decl.Middleware, log)
	if err != nil {
		return nil, err
	}
	s.grpc = grpc.NewServer(grpc.UnknownServiceHandler(s.handle), grpc.ChainStreamInterceptor(interceptors...))
	healthpb.RegisterHealthServer(s.grpc, hs)
	opts := reflection.ServerOptions{Services: s, DescriptorResolver: resolver{files}}
	reflectionv1.RegisterServerReflectionServer(s.grpc, reflection.NewServerV1(opts))
	reflectionv1alpha.RegisterServerReflectionServer(s.grpc, reflection.NewServer(opts))

	s.ln, err = net.Listen("tcp", decl.Listen)
	if err != nil {
		return nil, err
	}
	return s, nil
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

type phpResult struct {
	OK      bool            `json:"ok"`
	Message json.RawMessage `json:"message"`
	Code    int             `json:"code"`
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
	ev := map[string]any{
		"type":    "grpc",
		"service": m.serviceID,
		"method":  m.name,
	}
	if v := middleware.Values(stream.Context()); len(v) > 0 {
		ev["values"] = v
	}
	// A client-streaming method's messages are read by PHP through its
	// stream; every other method starts with one request message.
	if !m.desc.IsStreamingClient() {
		in := dynamicpb.NewMessage(m.desc.Input())
		if err := stream.RecvMsg(in); err != nil {
			return err
		}
		reqJSON, err := protojson.MarshalOptions{UseProtoNames: false, EmitUnpopulated: true}.Marshal(in)
		if err != nil {
			return status.Errorf(codes.Internal, "encode request: %v", err)
		}
		ev["message"] = json.RawMessage(reqJSON)
	}
	if m.desc.IsStreamingClient() || m.desc.IsStreamingServer() {
		id := openStream(stream, m.desc)
		defer closeStream(id)
		ev["stream"] = id
		ev["client_streaming"] = m.desc.IsStreamingClient()
		ev["server_streaming"] = m.desc.IsStreamingServer()
	}

	ctx := stream.Context()
	if s.maxDuration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.maxDuration)
		defer cancel()
	}
	raw, err := s.php.Dispatch(ctx, ev)
	if err != nil {
		if ctx.Err() == nil {
			// The PHP thread died with the call (a fatal error).
			return status.Error(codes.Internal, err.Error())
		}
		return status.FromContextError(err).Err()
	}
	var res phpResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return status.Errorf(codes.Internal, "decode php response: %v", err)
	}
	if !res.OK {
		var msg string
		_ = json.Unmarshal(res.Message, &msg)
		return status.Error(codes.Code(res.Code), msg)
	}
	if m.desc.IsStreamingServer() {
		return nil // PHP sent the replies through the stream
	}
	out := dynamicpb.NewMessage(m.desc.Output())
	if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(res.Message, out); err != nil {
		s.log.Error("php returned a message that does not match the proto", "method", full, "err", err)
		return status.Errorf(codes.Internal, "handler returned an invalid %s: %v", m.desc.Output().FullName(), err)
	}
	return stream.SendMsg(out)
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
	if err := files.RegisterFile(f); err != nil && !strings.Contains(err.Error(), "already registered") {
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

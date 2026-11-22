package sapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/cairophp/cairo/internal/core"
	"github.com/cairophp/cairo/internal/grpcserver"
	"github.com/cairophp/cairo/internal/wire"
)

// Cairo\Grpc\Client: unary calls through grpc-go. A message crosses as the
// array PHP has for it, which is protojson's, and is converted to and from
// protobuf's bytes with the descriptors of the .proto files (or the
// descriptor set a generated client carries), as the server's are
// (grpcserver.Message). Connections are shared by all PHP threads, one per
// target and TLS setting, and multiplex calls over HTTP/2.

func init() {
	registerPHP("grpc_client.php")
	core.RegisterOp("grpc.call", func(ctx context.Context, payload []byte) (any, error) {
		var req grpcCallRequest
		if err := wire.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		return grpcCall(ctx, req), nil
	})
}

type grpcClientTLS struct {
	Enabled    bool   `json:"enabled"`
	CAFile     string `json:"cafile"`
	Cert       string `json:"cert"`
	Key        string `json:"key"`
	ServerName string `json:"server_name"`
	Verify     bool   `json:"verify"`
}

type grpcCallRequest struct {
	Target      string              `json:"target"`
	Method      string              `json:"method"` // "/pkg.Service/Method"
	Protos      []string            `json:"protos"`
	Descriptors string              `json:"descriptors"` // base64 FileDescriptorSet
	Message     wire.Raw            `json:"message"`     // PHP's array for the request
	Metadata    map[string][]string `json:"metadata"`
	Timeout     float64             `json:"timeout"`
	TLS         grpcClientTLS       `json:"tls"`
}

type grpcConnKey struct {
	target string
	tls    grpcClientTLS
}

var (
	grpcConnsMu sync.Mutex
	grpcConns   = map[grpcConnKey]*grpc.ClientConn{}
	grpcFilesMu sync.Mutex
	grpcFiles   = map[string]*protoregistry.Files{}
	grpcMethods sync.Map // grpcMethodKey -> *grpcMethod
)

// grpcMethod is what a call needs of its method. A client makes the same
// calls again and again, so it is worked out once for a client's
// descriptors and a method's name.
type grpcMethod struct {
	path    string // "/pkg.Service/Method"
	desc    protoreflect.MethodDescriptor
	in, out grpcserver.Message
}

type grpcMethodKey struct{ protos, descriptors, method string }

func grpcConn(target string, o grpcClientTLS) (*grpc.ClientConn, error) {
	key := grpcConnKey{target, o}
	grpcConnsMu.Lock()
	defer grpcConnsMu.Unlock()
	if c, ok := grpcConns[key]; ok {
		return c, nil
	}
	creds := insecure.NewCredentials()
	if o.Enabled {
		cfg := &tls.Config{ServerName: o.ServerName, InsecureSkipVerify: !o.Verify}
		if o.CAFile != "" {
			pem, err := os.ReadFile(o.CAFile)
			if err != nil {
				return nil, fmt.Errorf("cafile: %w", err)
			}
			cfg.RootCAs = x509.NewCertPool()
			if !cfg.RootCAs.AppendCertsFromPEM(pem) {
				return nil, errors.New("cafile holds no PEM certificates")
			}
		}
		if o.Cert != "" {
			k := o.Key
			if k == "" {
				k = o.Cert
			}
			cert, err := tls.LoadX509KeyPair(o.Cert, k)
			if err != nil {
				return nil, fmt.Errorf("client certificate: %w", err)
			}
			cfg.Certificates = []tls.Certificate{cert}
		}
		creds = credentials.NewTLS(cfg)
	}
	// Requests and replies are frames: the bytes grpcserver.Message made
	// and reads.
	frames := grpc.WithDefaultCallOptions(grpc.ForceCodecV2(grpcserver.NewCodec()))
	c, err := grpc.NewClient(target, grpc.WithTransportCredentials(creds), frames)
	if err != nil {
		return nil, err
	}
	grpcConns[key] = c
	return c, nil
}

// grpcRegistry returns the descriptors for a call, compiled once per set of
// .proto files or descriptor set.
func grpcRegistry(protos []string, descriptors string) (*protoregistry.Files, error) {
	sum := sha256.Sum256([]byte(strings.Join(protos, "\x00") + "\x01" + descriptors))
	key := string(sum[:])
	grpcFilesMu.Lock()
	defer grpcFilesMu.Unlock()
	if f, ok := grpcFiles[key]; ok {
		return f, nil
	}
	files := new(protoregistry.Files)
	if descriptors != "" {
		raw, err := base64.StdEncoding.DecodeString(descriptors)
		if err != nil {
			return nil, fmt.Errorf("descriptors: %w", err)
		}
		var set descriptorpb.FileDescriptorSet
		if err := proto.Unmarshal(raw, &set); err != nil {
			return nil, fmt.Errorf("descriptors: %w", err)
		}
		if files, err = protodesc.NewFiles(&set); err != nil {
			return nil, fmt.Errorf("descriptors: %w", err)
		}
	}
	if len(protos) > 0 {
		compiled, err := grpcserver.CompileProtos(protos)
		if err != nil {
			return nil, err
		}
		var rerr error
		compiled.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
			if _, err := files.FindFileByPath(fd.Path()); err == nil {
				return true
			}
			if err := files.RegisterFile(fd); err != nil {
				rerr = err
				return false
			}
			return true
		})
		if rerr != nil {
			return nil, rerr
		}
	}
	grpcFiles[key] = files
	return files, nil
}

func grpcFail(code int, msg string) map[string]any {
	return map[string]any{"ok": false, "code": code, "error": msg}
}

// grpcMethodOf finds the method a request names, or the status that the
// call fails with. Only a method that exists is remembered, so what PHP
// passes as a name cannot grow the table.
func grpcMethodOf(req *grpcCallRequest) (*grpcMethod, map[string]any) {
	key := grpcMethodKey{strings.Join(req.Protos, "\x00"), req.Descriptors, req.Method}
	if m, ok := grpcMethods.Load(key); ok {
		return m.(*grpcMethod), nil
	}
	files, err := grpcRegistry(req.Protos, req.Descriptors)
	if err != nil {
		return nil, grpcFail(13, err.Error()) // INTERNAL
	}
	name := strings.TrimPrefix(req.Method, "/")
	svc, meth, ok := strings.Cut(name, "/")
	if !ok {
		return nil, grpcFail(12, "method must look like /package.Service/Method") // UNIMPLEMENTED
	}
	d, err := files.FindDescriptorByName(protoreflect.FullName(svc))
	sd, isSvc := d.(protoreflect.ServiceDescriptor)
	if err != nil || !isSvc {
		return nil, grpcFail(12, "service "+svc+" is not in the client's descriptors")
	}
	md := sd.Methods().ByName(protoreflect.Name(meth))
	if md == nil {
		return nil, grpcFail(12, "method "+meth+" is not in "+svc)
	}
	m := &grpcMethod{path: "/" + name, desc: md, in: grpcserver.NewMessage(md.Input()), out: grpcserver.NewMessage(md.Output())}
	grpcMethods.Store(key, m)
	return m, nil
}

// request returns the encoded request for the array PHP gave. No array, or
// an empty one, is the message with nothing set.
func (m *grpcMethod) request(value wire.Raw) ([]byte, error) {
	if len(value) == 0 || value[0] == wire.TagNull || bytes.Equal(value, emptyList) {
		return nil, nil
	}
	in, err := m.in.FromPHP(value)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "request does not match %s: %v", m.desc.Input().FullName(), err)
	}
	return in, nil
}

var emptyList = wire.AppendList(nil, 0)

// reply returns PHP's array for an encoded reply.
func (m *grpcMethod) reply(data []byte) (wire.Raw, error) {
	msg, err := m.out.ToPHP(make([]byte, 0, 2*len(data)+64), data)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "grpc: failed to unmarshal the received message: %v", err)
	}
	return msg, nil
}

// grpcCall makes one unary call. The result has "ok"; a reply carries
// "message" (PHP's array for it), "headers" and "trailers"; a failure
// carries the status "code" and "error".
func grpcCall(ctx context.Context, req grpcCallRequest) map[string]any {
	m, fail := grpcMethodOf(&req)
	if fail != nil {
		return fail
	}
	if m.desc.IsStreamingClient() || m.desc.IsStreamingServer() {
		return grpcFail(12, req.Method+" is a streaming method")
	}
	in, err := m.request(req.Message)
	if err != nil {
		return grpcStatusFail(err)
	}
	conn, err := grpcConn(req.Target, req.TLS)
	if err != nil {
		return grpcFail(14, err.Error()) // UNAVAILABLE
	}
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.Timeout*float64(time.Second)))
		defer cancel()
	}
	if len(req.Metadata) > 0 {
		ctx = metadata.NewOutgoingContext(ctx, metadata.MD(lowerKeys(req.Metadata)))
	}
	var out grpcserver.Frame
	var header, trailer metadata.MD
	err = conn.Invoke(ctx, m.path, &grpcserver.Frame{Data: in}, &out, grpc.Header(&header), grpc.Trailer(&trailer))
	if err != nil {
		st := status.Convert(err)
		res := grpcFail(int(st.Code()), st.Message())
		res["trailers"] = mdMap(trailer)
		return res
	}
	msg, err := m.reply(out.Data)
	if err != nil {
		return grpcStatusFail(err)
	}
	return map[string]any{"ok": true, "message": msg, "headers": mdMap(header), "trailers": mdMap(trailer)}
}

func lowerKeys(m map[string][]string) map[string][]string {
	out := make(map[string][]string, len(m))
	for k, v := range m {
		out[strings.ToLower(k)] = v
	}
	return out
}

func mdMap(md metadata.MD) map[string][]string {
	out := map[string][]string{}
	for k, v := range md {
		out[k] = v
	}
	return out
}

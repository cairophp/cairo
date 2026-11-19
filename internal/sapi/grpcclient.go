package sapi

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/cairophp/cairo/internal/grpcserver"
)

// Cairo\Grpc\Client: unary calls through grpc-go. Messages cross as
// protojson, encoded and decoded with descriptors from the .proto files (or
// the descriptor set a generated client carries). Connections are shared by
// all PHP threads, one per target and TLS setting, and multiplex calls over
// HTTP/2.

func init() {
	registerPHP("grpc_client.php")
	registerOp("grpc.call", func(ctx context.Context, payload []byte) (any, error) {
		var req grpcCallRequest
		if err := json.Unmarshal(payload, &req); err != nil {
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
	Message     json.RawMessage     `json:"message"`
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
)

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
	c, err := grpc.NewClient(target, grpc.WithTransportCredentials(creds))
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

// grpcCall makes one unary call. The result has "ok"; a reply carries
// "message" (protojson), "headers" and "trailers"; a failure carries the
// status "code" and "error".
func grpcCall(ctx context.Context, req grpcCallRequest) map[string]any {
	files, err := grpcRegistry(req.Protos, req.Descriptors)
	if err != nil {
		return grpcFail(13, err.Error()) // INTERNAL
	}
	name := strings.TrimPrefix(req.Method, "/")
	svc, meth, ok := strings.Cut(name, "/")
	if !ok {
		return grpcFail(12, "method must look like /package.Service/Method") // UNIMPLEMENTED
	}
	d, err := files.FindDescriptorByName(protoreflect.FullName(svc))
	sd, isSvc := d.(protoreflect.ServiceDescriptor)
	if err != nil || !isSvc {
		return grpcFail(12, "service "+svc+" is not in the client's descriptors")
	}
	md := sd.Methods().ByName(protoreflect.Name(meth))
	if md == nil {
		return grpcFail(12, "method "+meth+" is not in "+svc)
	}
	if md.IsStreamingClient() || md.IsStreamingServer() {
		return grpcFail(12, req.Method+" is a streaming method")
	}
	in := dynamicpb.NewMessage(md.Input())
	if len(req.Message) > 0 && string(req.Message) != "null" && string(req.Message) != "[]" {
		if err := protojson.Unmarshal(req.Message, in); err != nil {
			return grpcFail(3, "request does not match "+string(md.Input().FullName())+": "+err.Error()) // INVALID_ARGUMENT
		}
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
	out := dynamicpb.NewMessage(md.Output())
	var header, trailer metadata.MD
	err = conn.Invoke(ctx, "/"+name, in, out, grpc.Header(&header), grpc.Trailer(&trailer))
	if err != nil {
		st := status.Convert(err)
		res := grpcFail(int(st.Code()), st.Message())
		res["trailers"] = mdMap(trailer)
		return res
	}
	msg, err := protojson.MarshalOptions{EmitUnpopulated: true}.Marshal(out)
	if err != nil {
		return grpcFail(13, err.Error())
	}
	return map[string]any{"ok": true, "message": json.RawMessage(msg), "headers": mdMap(header), "trailers": mdMap(trailer)}
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

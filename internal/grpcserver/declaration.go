package grpcserver

import (
	"fmt"

	"github.com/cairophp/cairo/internal/middleware"
	"github.com/cairophp/cairo/internal/spec"
)

// Declaration is one `$app->grpc(':9000')` declaration.
type Declaration struct {
	Listen      string                   `json:"listen"`
	Protos      []string                 `json:"protos"`
	MaxDuration float64                  `json:"max_duration"`
	Services    []Service                `json:"services"`
	Middleware  []middleware.Declaration `json:"middleware"`
}

// Service maps a fully qualified gRPC service name to a PHP handler id.
type Service struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// Declared reads the gRPC servers out of what server.php declared and checks
// them before anything is served. The listen addresses and handler ids they
// take go into claims.
func Declared(s *spec.Spec, claims *spec.Claims) ([]Declaration, error) {
	var decls []Declaration
	if err := s.Section("grpc", &decls); err != nil {
		return nil, err
	}
	for _, d := range decls {
		if err := claims.Listen(d.Listen); err != nil {
			return nil, err
		}
		if len(d.Services) > 0 && len(d.Protos) == 0 {
			return nil, fmt.Errorf("gRPC server %s has services but no ->proto(...) files", d.Listen)
		}
		for _, sv := range d.Services {
			if err := claims.Handler(sv.ID); err != nil {
				return nil, err
			}
		}
	}
	return decls, nil
}

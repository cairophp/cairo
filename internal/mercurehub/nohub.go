//go:build nomercure

package mercurehub

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
)

// Hub is never made in this build.
type Hub struct{}

// Start refuses: the hub is not in a binary built with the tag nomercure.
func Start(Declaration, *slog.Logger, prometheus.Registerer) (*Hub, error) {
	return nil, errors.New("mercure: this cairo was built without the Mercure hub (build tag nomercure)")
}

func (*Hub) Routes(next http.Handler) http.Handler { return next }

func (*Hub) Stop() {}

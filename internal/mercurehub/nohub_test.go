//go:build nomercure

package mercurehub

import (
	"log/slog"
	"strings"
	"testing"
)

func TestNoHubInThisBuild(t *testing.T) {
	hub, err := Start(Declaration{Anonymous: true, DispatchTimeout: 5}, slog.New(slog.DiscardHandler), nil)
	if hub != nil || err == nil || !strings.Contains(err.Error(), "nomercure") {
		t.Errorf("Start = %v, %v", hub, err)
	}
	hub.Stop()
}

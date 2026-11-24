//go:build !nomercure

package sapi

import (
	"context"
	"fmt"

	"github.com/cairophp/cairo/internal/core"
	"github.com/cairophp/cairo/internal/mercurehub"
	"github.com/cairophp/cairo/internal/wire"
)

// mercure_publish(): an update goes to the hub of the process without
// HTTP and without a token. The hub hands out on the operation's
// goroutine, so the PHP thread serves on meanwhile. A build without the
// hub (nomercure) has no such function.
func init() {
	registerPHP("mercure_publish.php")
	core.RegisterOp("mercure.publish", func(ctx context.Context, payload []byte) (any, error) {
		var update mercurehub.Update
		if err := wire.Unmarshal(payload, &update); err != nil {
			return nil, fmt.Errorf("mercure.publish: %w", err)
		}
		id, err := mercurehub.Publish(ctx, update)
		if err != nil && mercurehub.Invalid(err) {
			// The caller's mistake, which PHP throws as a ValueError.
			return map[string]string{"invalid": err.Error()}, nil
		}
		if err != nil {
			return nil, err
		}
		return map[string]string{"id": id}, nil
	})
}

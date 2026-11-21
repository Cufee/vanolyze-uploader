//go:build !windows

package upload

import (
	"context"
	"log/slog"
)

func registerAutostart() error {
	return nil
}

func runTray(
	ctx context.Context,
	_ context.CancelFunc,
	_ *Status,
	_ string,
	_ string,
	_ *slog.Logger,
	_ chan<- struct{},
	run func(context.Context, <-chan struct{}) error,
) error {
	return run(ctx, nil)
}

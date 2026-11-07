//go:build !windows

// Package service runs a process as a native Windows service or as a
// foreground process with signal-driven cancellation elsewhere.
package service

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// Run starts run with an owned lifetime context. The callback must only be
// invoked after the service has bound its actual listener or resource.
func Run(name string, run func(context.Context, func()) error) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return run(ctx, func() {})
}

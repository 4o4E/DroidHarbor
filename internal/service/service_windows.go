//go:build windows

package service

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/windows/svc"
)

func Run(name string, run func(context.Context, func()) error) error {
	isService, err := svc.IsWindowsService()
	if err != nil {
		return err
	}
	if !isService {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return run(ctx, func() {})
	}
	return svc.Run(name, serviceHandler{run: run})
}

type serviceHandler struct {
	run func(context.Context, func()) error
}

func (h serviceHandler) Execute(_ []string, requests <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	done := make(chan error, 1)
	var readyOnce sync.Once
	go func() {
		done <- h.run(ctx, func() { readyOnce.Do(func() { close(ready) }) })
	}()
	var checkpoint uint32 = 1
	status := svc.Status{State: svc.StartPending, WaitHint: uint32(10 * time.Second / time.Millisecond), CheckPoint: checkpoint}
	changes <- status
	readySignal := (<-chan struct{})(ready)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-readySignal:
			status = svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
			changes <- status
			readySignal = nil
		case err := <-done:
			if err != nil {
				return false, 1
			}
			return false, 0
		case request, ok := <-requests:
			if !ok {
				cancel()
				err := <-done
				return false, exitCode(err)
			}
			switch request.Cmd {
			case svc.Interrogate:
				changes <- status
			case svc.Stop, svc.Shutdown:
				checkpoint = 1
				changes <- svc.Status{State: svc.StopPending, WaitHint: uint32(30 * time.Second / time.Millisecond), CheckPoint: checkpoint}
				cancel()
				for {
					select {
					case err := <-done:
						return false, exitCode(err)
					case <-ticker.C:
						checkpoint++
						changes <- svc.Status{State: svc.StopPending, WaitHint: uint32(30 * time.Second / time.Millisecond), CheckPoint: checkpoint}
					}
				}
			default:
			}
		case <-ticker.C:
			if readySignal != nil {
				checkpoint++
				status.CheckPoint = checkpoint
				changes <- status
			}
		}
	}
}

func exitCode(err error) uint32 {
	if err != nil && !errors.Is(err, context.Canceled) {
		return 1
	}
	return 0
}

//go:build linux

package main

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
)

type ownedADBServer struct {
	cmd  *exec.Cmd
	pid  int
	born string
	done chan struct{}
	mu   sync.Mutex
	err  error
}

type processStartResult struct {
	process *ownedADBServer
	err     error
}

func startOwnedADBServer(binary string, args, environment []string) (*ownedADBServer, error) {
	started := make(chan processStartResult, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		cmd := exec.Command(binary, args...)
		cmd.Env = environment
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		cmd.Stdin = nil
		cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
		if err := cmd.Start(); err != nil {
			started <- processStartResult{err: err}
			return
		}
		birth, err := processBirthIdentity(cmd.Process.Pid)
		if err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			started <- processStartResult{err: err}
			return
		}
		process := &ownedADBServer{cmd: cmd, pid: cmd.Process.Pid, born: birth, done: make(chan struct{})}
		started <- processStartResult{process: process}
		err = cmd.Wait()
		process.mu.Lock()
		process.err = err
		close(process.done)
		process.mu.Unlock()
	}()
	result := <-started
	return result.process, result.err
}

func (p *ownedADBServer) wait() error {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func (p *ownedADBServer) stop() error {
	select {
	case <-p.done:
		return p.wait()
	default:
	}
	killErr := p.cmd.Process.Kill()
	waitErr := p.wait()
	if killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
		return killErr
	}
	if waitErr != nil {
		if exit, ok := waitErr.(*exec.ExitError); !ok || exit.ExitCode() != -1 {
			return waitErr
		}
	}
	return nil
}

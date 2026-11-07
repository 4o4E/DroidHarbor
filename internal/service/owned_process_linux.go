//go:build linux

package service

import (
	"io"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
)

type linuxOwnedProcess struct {
	cmd  *exec.Cmd
	done chan struct{}
	mu   sync.Mutex
	err  error
}

func startOwnedProcess(path string, args, environment []string, stderr io.Writer) (OwnedProcess, error) {
	cmd := exec.Command(path, args...)
	cmd.Env = environment
	cmd.Stdout = io.Discard
	cmd.Stderr = stderr
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	process := &linuxOwnedProcess{cmd: cmd, done: make(chan struct{})}
	started := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := cmd.Start(); err != nil {
			process.finish(err)
			started <- err
			return
		}
		started <- nil
		process.finish(cmd.Wait())
	}()
	if err := <-started; err != nil {
		return nil, err
	}
	return process, nil
}

func (p *linuxOwnedProcess) finish(err error) {
	p.mu.Lock()
	p.err = err
	close(p.done)
	p.mu.Unlock()
}

func (p *linuxOwnedProcess) Wait() error {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func (p *linuxOwnedProcess) Kill() error {
	select {
	case <-p.done:
		return os.ErrProcessDone
	default:
		return p.cmd.Process.Kill()
	}
}

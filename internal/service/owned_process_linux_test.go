//go:build linux

package service

import (
	"errors"
	"io"
	"os"
	"runtime"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestOwnedChildSurvivesCallingThreadExit(t *testing.T) {
	// Keep the test goroutine on its original thread so the process starter
	// cannot accidentally be assigned to M0.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	mainTID := syscall.Gettid()
	type launched struct {
		process OwnedProcess
		tid     int
		err     error
	}
	started := make(chan launched, 1)
	var claimed atomic.Bool
	for i := 0; i < 2; i++ {
		go func() {
			runtime.LockOSThread()
			tid := syscall.Gettid()
			if tid == mainTID {
				runtime.UnlockOSThread()
				started <- launched{err: errors.New("starter was assigned to M0")}
				return
			}
			if !claimed.CompareAndSwap(false, true) {
				runtime.UnlockOSThread()
				return
			}
			process, err := StartOwnedProcess("/bin/sleep", []string{"30"}, os.Environ(), io.Discard)
			started <- launched{process: process, tid: tid, err: err}
			// Deliberately return while still locked: the starter OS thread
			// must exit when its goroutine ends.
		}()
	}
	result := <-started
	if result.err != nil {
		t.Fatal(result.err)
	}
	t.Cleanup(func() {
		_ = result.process.Kill()
		_ = result.process.Wait()
	})
	if result.tid == mainTID {
		t.Fatal("process starter unexpectedly ran on the locked test thread")
	}
	threadPath := "/proc/self/task/" + strconv.Itoa(result.tid)
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, err := os.Stat(threadPath)
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("process starter thread did not exit")
		}
		time.Sleep(time.Millisecond)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- result.process.Wait() }()
	select {
	case err := <-stopped:
		t.Fatalf("owned child died with the calling thread while its process is alive: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
}

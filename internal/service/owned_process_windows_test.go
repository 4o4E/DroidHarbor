package service

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const ownedProcessChildEnv = "DROIDHARBOR_OWNED_PROCESS_CHILD"
const ownedProcessGateEnv = "DROIDHARBOR_OWNED_PROCESS_GATE"
const ownedProcessStderrMarker = "OWNED_PROCESS_EXIT_WITNESS"

// TestOwnedProcessWindowsChildHelper runs only in the child test process.
func TestOwnedProcessWindowsChildHelper(t *testing.T) {
	if os.Getenv(ownedProcessChildEnv) != "1" {
		return
	}
	if _, err := io.WriteString(os.Stderr, ownedProcessStderrMarker); err != nil {
		t.Fatal(err)
	}
	gatePath := os.Getenv(ownedProcessGateEnv)
	for {
		_, err := os.Stat(gatePath)
		if err == nil {
			return
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type blockingProcessStderr struct {
	mu          sync.Mutex
	suffix      string
	entered     chan struct{}
	release     chan struct{}
	once        sync.Once
	unblockOnce sync.Once
}

func (w *blockingProcessStderr) unblock() {
	w.unblockOnce.Do(func() { close(w.release) })
}

func (w *blockingProcessStderr) Write(p []byte) (int, error) {
	w.mu.Lock()
	text := w.suffix + string(p)
	if len(text) >= len(ownedProcessStderrMarker) {
		w.suffix = text[len(text)-len(ownedProcessStderrMarker)+1:]
	} else {
		w.suffix = text
	}
	found := strings.Contains(text, ownedProcessStderrMarker)
	w.mu.Unlock()
	if found {
		w.once.Do(func() { close(w.entered) })
		<-w.release
	}
	return len(p), nil
}

func TestOwnedProcessKillAfterExitWhileStderrDrainBlocks(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "droidharbor-owned-process-test-")
	if err != nil {
		t.Fatal(err)
	}
	gatePath := filepath.Join(tempDir, "release")
	releaseGate := func() error {
		return os.WriteFile(gatePath, []byte("release"), 0600)
	}
	stderr := &blockingProcessStderr{entered: make(chan struct{}), release: make(chan struct{})}
	process, err := StartOwnedProcess(os.Args[0], []string{"-test.run=^TestOwnedProcessWindowsChildHelper$"},
		[]string{ownedProcessChildEnv + "=1", ownedProcessGateEnv + "=" + gatePath}, stderr)
	if err != nil {
		_ = os.RemoveAll(tempDir)
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if err := releaseGate(); err != nil {
			t.Errorf("release child gate during cleanup: %v", err)
		}
		stderr.unblock()
		if !waited {
			_ = process.Kill()
			_ = process.Wait()
		}
		_ = os.RemoveAll(tempDir)
	}()

	owned, ok := process.(*windowsOwnedProcess)
	if !ok {
		t.Fatal("StartOwnedProcess returned an unexpected implementation")
	}
	owned.mu.Lock()
	processHandle := owned.process
	var witness windows.Handle
	err = windows.DuplicateHandle(windows.CurrentProcess(), processHandle, windows.CurrentProcess(), &witness, 0, false, windows.DUPLICATE_SAME_ACCESS)
	owned.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(witness)

	select {
	case <-stderr.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("child did not write its stderr marker")
	}
	if err := releaseGate(); err != nil {
		t.Fatalf("release child gate: %v", err)
	}
	wait, err := windows.WaitForSingleObject(witness, 10*1000)
	if err != nil || wait != windows.WAIT_OBJECT_0 {
		t.Fatalf("duplicated process witness wait = (%d, %v), want signaled", wait, err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		var probe windows.Handle
		err = windows.DuplicateHandle(windows.CurrentProcess(), processHandle, windows.CurrentProcess(), &probe, 0, false, windows.DUPLICATE_SAME_ACCESS)
		if err == nil {
			_ = windows.CloseHandle(probe)
		}
		if err == windows.ERROR_INVALID_HANDLE {
			break
		}
		if err != nil {
			t.Fatalf("inspect original process handle: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("waiter did not release the original process handle while stderr drain was blocked")
		}
		time.Sleep(time.Millisecond)
	}

	if err := process.Kill(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("Kill after child exit = %v, want os.ErrProcessDone", err)
	}
	stderr.unblock()
	if err := process.Wait(); err != nil {
		t.Fatalf("Wait after successful child exit: %v", err)
	}
	waited = true
}

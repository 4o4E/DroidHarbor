//go:build windows

package service

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	procThreadAttributeJobList    = 0x0002000d
	procThreadAttributeHandleList = 0x00020002
)

type windowsOwnedProcess struct {
	process    windows.Handle
	job        windows.Handle
	done       chan struct{}
	stderrDone <-chan error
	mu         sync.Mutex
	err        error
}

func startOwnedProcess(path string, args, environment []string, stderr io.Writer) (OwnedProcess, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	var limits windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	attributes, err := windows.NewProcThreadAttributeList(2)
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	defer attributes.Delete()
	jobList := [1]windows.Handle{job}
	if err = attributes.Update(procThreadAttributeJobList, unsafe.Pointer(&jobList[0]), unsafe.Sizeof(jobList)); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}

	var pipeRead, pipeWrite windows.Handle
	if err = windows.CreatePipe(&pipeRead, &pipeWrite, nil, 0); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	cleanupPipe := func() { _ = windows.CloseHandle(pipeRead); _ = windows.CloseHandle(pipeWrite) }
	nulName, _ := windows.UTF16PtrFromString("NUL")
	nul, err := windows.CreateFile(nulName, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		cleanupPipe()
		_ = windows.CloseHandle(job)
		return nil, err
	}
	var inherited [3]windows.Handle
	for i, source := range []windows.Handle{nul, nul, pipeWrite} {
		if err = windows.DuplicateHandle(windows.CurrentProcess(), source, windows.CurrentProcess(), &inherited[i], 0, true, windows.DUPLICATE_SAME_ACCESS); err != nil {
			for _, handle := range inherited {
				if handle != 0 {
					_ = windows.CloseHandle(handle)
				}
			}
			_ = windows.CloseHandle(nul)
			cleanupPipe()
			_ = windows.CloseHandle(job)
			return nil, err
		}
	}
	_ = windows.CloseHandle(nul)
	_ = windows.CloseHandle(pipeWrite)
	pipeWrite = 0
	defer func() {
		for _, handle := range inherited {
			_ = windows.CloseHandle(handle)
		}
	}()
	if err = attributes.Update(procThreadAttributeHandleList, unsafe.Pointer(&inherited[0]), unsafe.Sizeof(inherited[0])*uintptr(len(inherited))); err != nil {
		cleanupPipe()
		_ = windows.CloseHandle(job)
		return nil, err
	}
	application, err := windows.UTF16PtrFromString(path)
	if err != nil {
		cleanupPipe()
		_ = windows.CloseHandle(job)
		return nil, err
	}
	commandLine, err := windows.UTF16FromString(joinWindowsCommandLine(append([]string{path}, args...)))
	if err != nil {
		cleanupPipe()
		_ = windows.CloseHandle(job)
		return nil, err
	}
	envBlock, err := windowsEnvironmentBlock(environment)
	if err != nil {
		cleanupPipe()
		_ = windows.CloseHandle(job)
		return nil, err
	}
	startup := windows.StartupInfoEx{ProcThreadAttributeList: attributes.List()}
	startup.StartupInfo.Cb = uint32(unsafe.Sizeof(startup))
	startup.StartupInfo.Flags = windows.STARTF_USESTDHANDLES
	startup.StartupInfo.StdInput, startup.StartupInfo.StdOutput, startup.StartupInfo.StdErr = inherited[0], inherited[1], inherited[2]
	var information windows.ProcessInformation
	err = windows.CreateProcess(application, &commandLine[0], nil, nil, true,
		windows.CREATE_UNICODE_ENVIRONMENT|windows.EXTENDED_STARTUPINFO_PRESENT, &envBlock[0], nil,
		(*windows.StartupInfo)(unsafe.Pointer(&startup)), &information)
	runtime.KeepAlive(&jobList)
	runtime.KeepAlive(&inherited)
	runtime.KeepAlive(envBlock)
	if err != nil {
		cleanupPipe()
		_ = windows.CloseHandle(job)
		return nil, err
	}
	if err = windows.CloseHandle(information.Thread); err != nil {
		return nil, abortStartedProcess(information.Process, information.Thread, job, pipeRead,
			fmt.Errorf("close child thread handle: %w", err))
	}
	reader := os.NewFile(uintptr(pipeRead), "adb-stderr")
	if reader == nil {
		return nil, abortStartedProcess(information.Process, 0, job, pipeRead,
			fmt.Errorf("could not open ADB diagnostic pipe"))
	}
	stderrDone := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(stderr, reader)
		closeErr := reader.Close()
		var result error
		if copyErr != nil {
			result = fmt.Errorf("copy ADB diagnostic pipe: %w", copyErr)
		}
		if closeErr != nil {
			result = errors.Join(result, fmt.Errorf("close ADB diagnostic pipe: %w", closeErr))
		}
		stderrDone <- result
	}()
	process := &windowsOwnedProcess{process: information.Process, job: job, done: make(chan struct{}), stderrDone: stderrDone}
	go process.waitForExit()
	return process, nil
}

func abortStartedProcess(process, thread, job, pipeRead windows.Handle, cause error) error {
	if process != 0 {
		if err := windows.TerminateProcess(process, 1); err != nil {
			cause = errors.Join(cause, fmt.Errorf("terminate child during startup cleanup: %w", err))
		}
	}
	if job != 0 {
		if err := windows.CloseHandle(job); err != nil {
			cause = errors.Join(cause, fmt.Errorf("close child process job during startup cleanup: %w", err))
		}
	}
	if process != 0 {
		if _, err := windows.WaitForSingleObject(process, windows.INFINITE); err != nil {
			cause = errors.Join(cause, fmt.Errorf("wait for child during startup cleanup: %w", err))
		}
	}
	for _, item := range []struct {
		handle windows.Handle
		name   string
	}{
		{thread, "child thread"},
		{pipeRead, "ADB diagnostic pipe"},
		{process, "child process"},
	} {
		if item.handle != 0 {
			if err := windows.CloseHandle(item.handle); err != nil {
				cause = errors.Join(cause, fmt.Errorf("close %s during startup cleanup: %w", item.name, err))
			}
		}
	}
	return cause
}

func (p *windowsOwnedProcess) waitForExit() {
	p.mu.Lock()
	processHandle := p.process
	p.mu.Unlock()

	_, waitErr := windows.WaitForSingleObject(processHandle, windows.INFINITE)
	var result error
	if waitErr != nil {
		result = waitErr
	} else {
		var code uint32
		if err := windows.GetExitCodeProcess(processHandle, &code); err != nil {
			result = err
		} else if code != 0 {
			result = fmt.Errorf("process exited with code %d", code)
		}
	}

	p.mu.Lock()
	processHandle, jobHandle := p.process, p.job
	p.process, p.job = 0, 0
	p.mu.Unlock()

	if processHandle != 0 {
		if err := windows.CloseHandle(processHandle); err != nil {
			result = errors.Join(result, fmt.Errorf("close process handle: %w", err))
		}
	}
	if jobHandle != 0 {
		if err := windows.CloseHandle(jobHandle); err != nil {
			result = errors.Join(result, fmt.Errorf("close process job handle: %w", err))
		}
	}
	result = errors.Join(result, <-p.stderrDone)

	p.mu.Lock()
	p.err = result
	close(p.done)
	p.mu.Unlock()
}

func (p *windowsOwnedProcess) Wait() error {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func (p *windowsOwnedProcess) Kill() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.done:
		return os.ErrProcessDone
	default:
	}
	processHandle := p.process
	if processHandle == 0 {
		return os.ErrProcessDone
	}
	wait, err := windows.WaitForSingleObject(processHandle, 0)
	if err != nil {
		return err
	}
	switch wait {
	case windows.WAIT_OBJECT_0:
		return os.ErrProcessDone
	case uint32(windows.WAIT_TIMEOUT):
		if err := windows.TerminateProcess(processHandle, 1); err != nil {
			wait, waitErr := windows.WaitForSingleObject(processHandle, 0)
			if waitErr == nil && wait == windows.WAIT_OBJECT_0 {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	default:
		return fmt.Errorf("unexpected process wait status %d", wait)
	}
}

func windowsEnvironmentBlock(environment []string) ([]uint16, error) {
	entries := append([]string(nil), environment...)
	sort.Slice(entries, func(i, j int) bool { return strings.ToUpper(entries[i]) < strings.ToUpper(entries[j]) })
	var block []uint16
	for _, entry := range entries {
		encoded, err := windows.UTF16FromString(entry)
		if err != nil {
			return nil, err
		}
		block = append(block, encoded[:len(encoded)-1]...)
		block = append(block, 0)
	}
	if len(block) == 0 {
		return []uint16{0, 0}, nil
	}
	return append(block, 0), nil
}

func joinWindowsCommandLine(arguments []string) string {
	quoted := make([]string, len(arguments))
	for i, argument := range arguments {
		quoted[i] = windows.EscapeArg(argument)
	}
	return strings.Join(quoted, " ")
}

//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

type ownedADBServer struct {
	process windows.Handle
	job     windows.Handle
	pid     uint32
	born    string
	done    chan struct{}
	mu      sync.Mutex
	err     error
	stopped bool
}

func startOwnedADBServer(binary string, args, environment []string) (*ownedADBServer, error) {
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
	attributes, err := createJobProcessAttributeList(job)
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	defer deleteProcThreadAttributeList(attributes)

	application, err := windows.UTF16PtrFromString(binary)
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	commandLine, err := windows.UTF16FromString(joinWindowsCommandLine(append([]string{binary}, args...)))
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	envBlock, err := windowsEnvironmentBlock(environment)
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	input, err := os.Open(os.DevNull)
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	defer input.Close()
	// Only these duplicated handles are inheritable; never leak the job handle.
	var inherited [2]windows.Handle
	for i, source := range []windows.Handle{windows.Handle(input.Fd()), windows.Handle(os.Stderr.Fd())} {
		if err = windows.DuplicateHandle(windows.CurrentProcess(), source, windows.CurrentProcess(), &inherited[i], 0, true, windows.DUPLICATE_SAME_ACCESS); err != nil {
			for _, handle := range inherited {
				if handle != 0 {
					_ = windows.CloseHandle(handle)
				}
			}
			_ = windows.CloseHandle(job)
			return nil, err
		}
	}
	defer func() {
		for _, handle := range inherited {
			_ = windows.CloseHandle(handle)
		}
	}()
	startup := windows.StartupInfoEx{ProcThreadAttributeList: attributes.List()}
	startup.StartupInfo.Cb = uint32(unsafe.Sizeof(startup))
	startup.StartupInfo.Flags = windows.STARTF_USESTDHANDLES
	startup.StartupInfo.StdInput = inherited[0]
	startup.StartupInfo.StdOutput = inherited[1]
	startup.StartupInfo.StdErr = inherited[1]
	handles := inherited[:]
	if err = attributes.Update(procThreadAttributeHandleList, unsafe.Pointer(&handles[0]), unsafe.Sizeof(handles[0])*uintptr(len(handles))); err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("UpdateProcThreadAttribute handle list failed: %w", err)
	}
	var information windows.ProcessInformation
	if err = windows.CreateProcess(application, &commandLine[0], nil, nil, true,
		windows.CREATE_UNICODE_ENVIRONMENT|windows.EXTENDED_STARTUPINFO_PRESENT, &envBlock[0], nil,
		(*windows.StartupInfo)(unsafe.Pointer(&startup)), &information); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	runtime.KeepAlive(handles)
	runtime.KeepAlive(envBlock)
	runtime.KeepAlive(attributes)
	born, err := processBirthIdentityHandle(information.Process)
	if err != nil {
		return nil, errors.Join(err, abortCreatedADBProcess(information, job))
	}
	threadCloseErr := windows.CloseHandle(information.Thread)
	if threadCloseErr != nil {
		return nil, errors.Join(threadCloseErr, abortCreatedADBProcess(information, job))
	}
	information.Thread = 0
	process := &ownedADBServer{process: information.Process, job: job, pid: information.ProcessId, born: born, done: make(chan struct{})}
	go process.waitForExit()
	return process, nil
}

const (
	procThreadAttributeJobList    = 0x0002000d
	procThreadAttributeHandleList = 0x00020002
)

func createJobProcessAttributeList(job windows.Handle) (*windows.ProcThreadAttributeListContainer, error) {
	attributes, err := windows.NewProcThreadAttributeList(2)
	if err != nil {
		return nil, err
	}
	jobList := [1]windows.Handle{job}
	if err := attributes.Update(procThreadAttributeJobList, unsafe.Pointer(&jobList[0]), unsafe.Sizeof(jobList)); err != nil {
		attributes.Delete()
		return nil, fmt.Errorf("UpdateProcThreadAttribute job list failed: %w", err)
	}
	runtime.KeepAlive(&jobList)
	return attributes, nil
}

func deleteProcThreadAttributeList(list *windows.ProcThreadAttributeListContainer) {
	if list != nil {
		list.Delete()
	}
}

func abortCreatedADBProcess(information windows.ProcessInformation, job windows.Handle) error {
	var result error
	if err := windows.TerminateProcess(information.Process, 1); err != nil {
		result = errors.Join(result, fmt.Errorf("terminate ADB startup child: %w", err))
	}
	// Closing the owned job also terminates descendants if direct termination failed.
	if err := windows.CloseHandle(job); err != nil {
		result = errors.Join(result, fmt.Errorf("close ADB startup job: %w", err))
	}
	if _, err := windows.WaitForSingleObject(information.Process, windows.INFINITE); err != nil {
		result = errors.Join(result, fmt.Errorf("wait for ADB startup child: %w", err))
	}
	if information.Thread != 0 {
		if err := windows.CloseHandle(information.Thread); err != nil {
			result = errors.Join(result, fmt.Errorf("close ADB startup thread: %w", err))
		}
	}
	if err := windows.CloseHandle(information.Process); err != nil {
		result = errors.Join(result, fmt.Errorf("close ADB startup process: %w", err))
	}
	return result
}

func (p *ownedADBServer) waitForExit() {
	_, waitErr := windows.WaitForSingleObject(p.process, windows.INFINITE)
	var exitCode uint32
	if waitErr == nil {
		waitErr = windows.GetExitCodeProcess(p.process, &exitCode)
	}
	p.mu.Lock()
	if waitErr != nil {
		p.err = waitErr
	} else if exitCode != 0 && !p.stopped {
		p.err = fmt.Errorf("adb server exited with code %d", exitCode)
	}
	p.err = errors.Join(p.err, windows.CloseHandle(p.process), windows.CloseHandle(p.job))
	close(p.done)
	p.mu.Unlock()
}

func (p *ownedADBServer) wait() error {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func (p *ownedADBServer) stop() error {
	p.mu.Lock()
	select {
	case <-p.done:
		err := p.err
		p.mu.Unlock()
		return err
	default:
	}
	p.stopped = true
	err := windows.TerminateProcess(p.process, 1)
	p.mu.Unlock()
	if err != nil && !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		select {
		case <-p.done:
			return p.wait()
		default:
			return err
		}
	}
	<-p.done
	return nil
}

func windowsEnvironmentBlock(environment []string) ([]uint16, error) {
	entries := append([]string(nil), environment...)
	sort.Slice(entries, func(i, j int) bool {
		return strings.ToUpper(entries[i]) < strings.ToUpper(entries[j])
	})
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
	var result strings.Builder
	for i, argument := range arguments {
		if i != 0 {
			result.WriteByte(' ')
		}
		result.WriteString(windows.EscapeArg(argument))
	}
	return result.String()
}

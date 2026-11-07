//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

const stillActiveExitCode = 259

func processBirthIdentity(pid int) (string, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(handle)
	return processBirthIdentityHandle(handle)
}

func processBirthIdentityHandle(handle windows.Handle) (string, error) {
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		return "", err
	}
	return fmt.Sprintf("%08x%08x", creation.HighDateTime, creation.LowDateTime), nil
}

func processIdentityMatches(pid int, birth string) (bool, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return false, nil
		}
		return false, err
	}
	defer windows.CloseHandle(handle)
	var exitCode uint32
	if err = windows.GetExitCodeProcess(handle, &exitCode); err != nil {
		return false, err
	}
	if exitCode != stillActiveExitCode {
		return false, nil
	}
	current, err := processBirthIdentityHandle(handle)
	if err != nil {
		return false, err
	}
	return birth != "" && current == birth, nil
}

func secureTaskDirectory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("任务路径不是目录")
	}
	return setPrivateWindowsACL(path, true)
}

func protectPrivatePath(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	return setPrivateWindowsACL(path, info.IsDir())
}

func createPrivateDirectory(path string) error {
	if err := os.Mkdir(path, 0700); err != nil {
		return err
	}
	return secureTaskDirectory(path)
}

func setPrivateWindowsACL(path string, directory bool) error {
	user := windows.GetCurrentProcessToken()
	tokenUser, err := user.GetTokenUser()
	if err != nil {
		return err
	}
	sid := tokenUser.User.Sid
	inheritance := ""
	if directory {
		inheritance = "OICI"
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;" + inheritance + ";FA;;;" + sid.String() + ")")
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		sid, nil, dacl, nil)
}

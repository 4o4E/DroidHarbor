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
	current, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	if current == nil {
		return errors.New("任务路径没有 Windows 安全描述符")
	}
	owner, _, err := current.Owner()
	if err != nil {
		return err
	}
	information := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION)
	// 已属于当前用户时只收紧 DACL；重复设置 owner 仍要求普通目录可能没有的 WRITE_OWNER。
	if !owner.Equals(sid) {
		information |= windows.OWNER_SECURITY_INFORMATION
	}
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
		information,
		sid, nil, dacl, nil)
}

//go:build windows

package service

import (
	"errors"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// CurrentProfileDirectory uses the same CSIDL_PROFILE lookup used by the
// official Windows ADB server; HOME and USERPROFILE environment variables do
// not establish ADB's default authorization-key location.
func CurrentProfileDirectory() (string, error) {
	const csidlProfile = 0x28
	var path [260]uint16
	proc := syscall.NewLazyDLL("shell32.dll").NewProc("SHGetFolderPathW")
	hr, _, _ := proc.Call(0, csidlProfile, 0, 0, uintptr(unsafe.Pointer(&path[0])))
	if hr != 0 {
		return "", errors.New("SHGetFolderPathW(CSIDL_PROFILE) failed")
	}
	profile := windows.UTF16ToString(path[:])
	if profile == "" {
		return "", errors.New("Windows returned an empty service profile path")
	}
	return profile, nil
}

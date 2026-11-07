//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

func processBirthIdentity(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	stat := string(data)
	closeParen := strings.LastIndexByte(stat, ')')
	if closeParen < 0 || closeParen+1 >= len(stat) {
		return "", errors.New("无法读取 Linux 进程身份")
	}
	fields := strings.Fields(stat[closeParen+1:])
	if len(fields) <= 19 {
		return "", errors.New("Linux 进程身份数据无效")
	}
	if fields[0] == "Z" || fields[0] == "X" {
		return "", os.ErrProcessDone
	}
	if _, err = strconv.ParseUint(fields[19], 10, 64); err != nil {
		return "", errors.New("无法解析 Linux 进程创建时刻")
	}
	bootID, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", fmt.Errorf("无法读取 Linux boot identity: %w", err)
	}
	boot := strings.TrimSpace(string(bootID))
	if len(boot) != 36 || strings.Count(boot, "-") != 4 {
		return "", errors.New("Linux boot identity 无效")
	}
	return boot + ":" + fields[19], nil
}

func processIdentityMatches(pid int, birth string) (bool, error) {
	current, err := processBirthIdentity(pid)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
			return false, nil
		}
		return false, err
	}
	return birth != "" && current == birth, nil
}

func secureTaskDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("任务路径不是普通目录")
	}
	if info.Mode().Perm()&0077 != 0 {
		return errors.New("任务目录权限过宽；请设置为 0700")
	}
	return nil
}

func protectPrivatePath(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("私有路径不是普通文件")
	}
	if info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("私有路径权限过宽；请设置为 %04o", info.Mode().Perm()&0777&^0077)
	}
	return nil
}

func createPrivateDirectory(path string) error {
	if err := os.Mkdir(path, 0700); err != nil {
		return err
	}
	return secureTaskDirectory(path)
}

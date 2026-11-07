//go:build !windows

package service

import "os"

func CurrentProfileDirectory() (string, error) {
	return os.UserHomeDir()
}

//go:build !windows && !linux

package service

import (
	"errors"
	"io"
)

func startOwnedProcess(path string, args, environment []string, stderr io.Writer) (OwnedProcess, error) {
	return nil, errors.New("owned processes are unsupported on this platform")
}

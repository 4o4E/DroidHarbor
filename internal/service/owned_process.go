package service

import "io"

// OwnedProcess represents a child whose lifetime is tied to this process.
// Wait may be called once; Kill terminates only this exact child.
type OwnedProcess interface {
	Wait() error
	Kill() error
}

// StartOwnedProcess starts an owned child. Linux and Windows tie its lifetime
// to this process with Pdeathsig and a kill-on-close job, respectively.
func StartOwnedProcess(path string, args, environment []string, stderr io.Writer) (OwnedProcess, error) {
	return startOwnedProcess(path, args, environment, stderr)
}

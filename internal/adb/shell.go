package adb

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"strings"

	"droidharbor/internal/diagnostics"
)

const (
	shellStdin byte = iota
	shellStdout
	shellStderr
	shellExit
	shellCloseStdin
	shellWindowSize
	shellPacketHeader = 5
	shellMaxPacket    = 1024 * 1024
)

// ShellPacket is one shell_v2 packet. Data aliases scratch and remains valid only
// until the caller reuses or modifies scratch.
type ShellPacket struct {
	ID   byte
	Data []byte
}

// ReadShellPacket reads the shell_v2 one-byte ID, little-endian uint32 length,
// and bounded payload. scratch is grown only when the bounded packet requires it.
func ReadShellPacket(r io.Reader, scratch []byte, maxBytes uint32) (ShellPacket, error) {
	var header [shellPacketHeader]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return ShellPacket{}, fmt.Errorf("read shell_v2 header: %w", err)
	}
	id := header[0]
	if id != shellStdin && id != shellStdout && id != shellStderr && id != shellExit && id != shellCloseStdin && id != shellWindowSize {
		return ShellPacket{}, diagnostics.New("invalid shell_v2 packet id")
	}
	n := binary.LittleEndian.Uint32(header[1:])
	if n > maxBytes || n > shellMaxPacket {
		return ShellPacket{}, diagnostics.New("shell_v2 packet exceeds limit")
	}
	if id == shellExit && n != 1 {
		return ShellPacket{}, diagnostics.New("invalid shell_v2 EXIT packet length")
	}
	if id == shellCloseStdin && n != 0 {
		return ShellPacket{}, diagnostics.New("invalid shell_v2 CLOSE_STDIN packet length")
	}
	if uint64(n) > uint64(cap(scratch)) {
		scratch = make([]byte, int(n))
	} else {
		scratch = scratch[:int(n)]
	}
	if _, err := io.ReadFull(r, scratch); err != nil {
		return ShellPacket{}, fmt.Errorf("read shell_v2 payload: %w", err)
	}
	return ShellPacket{ID: id, Data: scratch}, nil
}

// ShellResult contains all bounded stdout/stderr bytes and the remote exit status.
type ShellResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode uint8
}

// RunShell runs a command without legacy shell fallback. The combined lengths of
// stdout and stderr never exceed maxOutput.
func (c *Client) RunShell(ctx context.Context, transportID uint64, command string, maxOutput int64, authorize func() error) (ShellResult, error) {
	if command == "" || strings.IndexByte(command, 0) >= 0 {
		return ShellResult{}, diagnostics.New("invalid shell command")
	}
	if maxOutput < 0 {
		return ShellResult{}, diagnostics.New("invalid shell output limit")
	}
	if authorize == nil {
		return ShellResult{}, diagnostics.New("ADB service authorization is required")
	}
	conn, err := c.Open(ctx, transportID, "shell,v2,raw:"+command, authorize)
	if err != nil {
		return ShellResult{}, err
	}
	defer conn.Close()
	if err := writeAll(conn, []byte{shellCloseStdin, 0, 0, 0, 0}); err != nil {
		return ShellResult{}, err
	}
	result := ShellResult{Stdout: make([]byte, 0), Stderr: make([]byte, 0)}
	var scratch []byte
	var used int64
	for {
		if err := ctx.Err(); err != nil {
			return ShellResult{}, err
		}
		packet, err := ReadShellPacket(conn, scratch, shellMaxPacket)
		if err != nil {
			return ShellResult{}, err
		}
		scratch = packet.Data
		switch packet.ID {
		case shellStdout, shellStderr:
			if int64(len(packet.Data)) > maxOutput-used {
				return ShellResult{}, diagnostics.New("shell_v2 output exceeds limit")
			}
			used += int64(len(packet.Data))
			if packet.ID == shellStdout {
				result.Stdout = append(result.Stdout, packet.Data...)
			} else {
				result.Stderr = append(result.Stderr, packet.Data...)
			}
		case shellExit:
			result.ExitCode = packet.Data[0]
			return result, nil
		default:
			return ShellResult{}, diagnostics.New("unexpected shell_v2 packet")
		}
	}
}

// ShellQuote returns one single-quoted POSIX shell argument; it is not a command sanitizer.
func ShellQuote(value string) (string, error) {
	if strings.IndexByte(value, 0) >= 0 {
		return "", diagnostics.New("shell argument contains NUL")
	}
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'", nil
}

package adb

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	syncSend      = "SEND"
	syncData      = "DATA"
	syncDone      = "DONE"
	syncOkay      = "OKAY"
	syncFail      = "FAIL"
	syncPathMax   = 1024
	syncDataMax   = 64 * 1024
	syncStatusMax = 64 * 1024
)

// Push streams source to remotePath using the ADB sync v1 SEND/DATA/DONE protocol.
// Progress reports bytes accepted by the local connection only; it is not evidence
// that the remote file was installed successfully.
func (c *Client) Push(ctx context.Context, transportID uint64, remotePath string, mode uint32, modified time.Time, source io.Reader, authorize func() error, progress func(int64)) error {
	if source == nil {
		return errors.New("ADB push source is required")
	}
	if remotePath == "" || len(remotePath) > syncPathMax || !utf8.ValidString(remotePath) || strings.IndexByte(remotePath, 0) >= 0 {
		return errors.New("invalid ADB sync path")
	}
	if mode&^uint32(07777) != 0 {
		return errors.New("invalid ADB sync mode")
	}
	if timestamp := modified.Unix(); timestamp < 0 || timestamp > int64(^uint32(0)) {
		return errors.New("ADB sync modification time is outside protocol range")
	}
	if authorize == nil {
		return errors.New("ADB service authorization is required")
	}
	pathAndMode := make([]byte, 0, len(remotePath)+1+10)
	pathAndMode = append(pathAndMode, remotePath...)
	pathAndMode = append(pathAndMode, ',')
	pathAndMode = strconv.AppendUint(pathAndMode, uint64(mode), 10)
	if len(pathAndMode) > syncPathMax {
		return errors.New("ADB sync path and mode exceed protocol limit")
	}
	conn, err := c.Open(ctx, transportID, "sync:", authorize)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := writeSyncRequest(conn, syncSend, pathAndMode); err != nil {
		return err
	}
	var chunk [syncDataMax]byte
	var sent int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := io.ReadFull(source, chunk[:])
		if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
			return fmt.Errorf("read ADB push source: %w", readErr)
		}
		if n > 0 {
			if err := writeSyncData(conn, chunk[:n], &sent, progress); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := writeSyncIDAndWord(conn, syncDone, uint32(modified.Unix())); err != nil {
		return err
	}
	return readSyncStatus(conn)
}

func writeSyncRequest(w io.Writer, id string, payload []byte) error {
	if len(payload) > syncDataMax && id == syncData {
		return errors.New("ADB sync data chunk exceeds protocol limit")
	}
	if err := writeSyncIDAndWord(w, id, uint32(len(payload))); err != nil {
		return err
	}
	return writeAll(w, payload)
}
func writeSyncData(w io.Writer, payload []byte, sent *int64, progress func(int64)) error {
	if len(payload) > syncDataMax {
		return errors.New("ADB sync data chunk exceeds protocol limit")
	}
	if err := writeSyncIDAndWord(w, syncData, uint32(len(payload))); err != nil {
		return err
	}
	for len(payload) > 0 {
		n, err := w.Write(payload)
		if n < 0 || n > len(payload) {
			return errors.New("invalid short write from ADB stream")
		}
		payload = payload[n:]
		*sent += int64(n)
		if n > 0 && progress != nil {
			progress(*sent)
		}
		if err != nil {
			return fmt.Errorf("write ADB stream: %w", err)
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func writeSyncIDAndWord(w io.Writer, id string, value uint32) error {
	var buf [8]byte
	copy(buf[:4], id)
	binary.LittleEndian.PutUint32(buf[4:], value)
	return writeAll(w, buf[:])
}

func readSyncStatus(r io.Reader) error {
	var status [8]byte
	if _, err := io.ReadFull(r, status[:]); err != nil {
		return fmt.Errorf("read ADB sync status: %w", err)
	}
	length := binary.LittleEndian.Uint32(status[4:])
	switch string(status[:4]) {
	case syncOkay:
		if length != 0 {
			return errors.New("ADB sync returned malformed OKAY status")
		}
		return nil
	case syncFail:
		if length > syncStatusMax {
			return errors.New("ADB sync failure status is oversized")
		}
		// Consume the device message to leave framing exact, but never surface its contents.
		if length != 0 {
			if _, err := io.CopyN(io.Discard, r, int64(length)); err != nil {
				return fmt.Errorf("read ADB sync failure status: %w", err)
			}
		}
		return errors.New("ADB sync push failed")
	default:
		return errors.New("ADB sync returned unknown status")
	}
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if n < 0 || n > len(data) {
			return errors.New("invalid short write from ADB stream")
		}
		data = data[n:]
		if err != nil {
			return fmt.Errorf("write ADB stream: %w", err)
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

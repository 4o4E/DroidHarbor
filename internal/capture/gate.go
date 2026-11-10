package capture

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"droidharbor/internal/adb"
	"droidharbor/internal/diagnostics"
)

type readyIdentity struct {
	pid   int
	boot  string
	ticks uint64
}

var errSocketNotListening = diagnostics.New("scrcpy abstract socket is not listening")

func startupCommand(jar, gateNonce string, scid int32, settings Settings) (string, error) {
	args, err := serverOptions(scid, settings)
	if err != nil {
		return "", err
	}
	// Android's app_process rejects --nice-name on tested Android 13/16
	// devices and exits before the server runs. Ownership verification uses
	// the JAR environment plus PID/start ticks instead of a process name.
	quoted := make([]string, 0, len(args)+4)
	quoted = append(quoted, "app_process", "/system/bin", "com.genymobile.scrcpy.Server", "5.0.1")
	quoted = append(quoted, args...)
	for i := range quoted {
		quoted[i] = "'" + strings.ReplaceAll(quoted[i], "'", "'\\''") + "'"
	}
	command := "CLASSPATH='" + jar + "'; export CLASSPATH; stat=$(cat /proc/$$/stat) || exit 1; rest=${stat##*) }; set -- $rest; ticks=${20}; boot=$(cat /proc/sys/kernel/random/boot_id) || exit 1; printf 'DHREADY %s %s %s\\n' \"$$\" \"$boot\" \"$ticks\"; IFS= read -r gate || exit 1; [ \"$gate\" = '" + gateNonce + "' ] || exit 1; IFS= read -r go || exit 1; [ \"$go\" = GO ] || exit 1; exec " + strings.Join(quoted, " ")
	return command, nil
}

func readReady(ctx context.Context, conn net.Conn) (readyIdentity, error) {
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetReadDeadline(deadline); err != nil {
			return readyIdentity{}, err
		}
		defer conn.SetReadDeadline(time.Time{})
	}
	var line []byte
	var scratch []byte
	for len(line) <= maxShellLine {
		packet, err := adb.ReadShellPacket(conn, scratch, maxShellLine)
		if err != nil {
			return readyIdentity{}, err
		}
		scratch = packet.Data
		switch packet.ID {
		case 1:
			for _, b := range packet.Data {
				if b == '\n' {
					return parseReadyLine(string(line))
				}
				line = append(line, b)
				if len(line) > maxShellLine {
					return readyIdentity{}, diagnostics.New("capture bootstrap line exceeds limit")
				}
			}
		case 2:
			continue
		case 3:
			return readyIdentity{}, diagnostics.New("foreground scrcpy shell exited before readiness")
		default:
			return readyIdentity{}, diagnostics.New("foreground shell sent an invalid shell-v2 packet")
		}
	}
	return readyIdentity{}, diagnostics.New("capture bootstrap line exceeds limit")
}

func parseReadyLine(line string) (readyIdentity, error) {
	parts := strings.Fields(line)
	if len(parts) != 4 || parts[0] != "DHREADY" {
		return readyIdentity{}, diagnostics.New("invalid capture bootstrap line")
	}
	pid, err := strconv.Atoi(parts[1])
	if err != nil || pid < 1 {
		return readyIdentity{}, diagnostics.New("invalid capture process PID")
	}
	ticks, err := strconv.ParseUint(parts[3], 10, 64)
	if err != nil || ticks == 0 {
		return readyIdentity{}, diagnostics.New("invalid capture process start time")
	}
	return readyIdentity{pid: pid, boot: parts[2], ticks: ticks}, nil
}

func writeShellStdin(conn net.Conn, payload []byte) error { return writeShellPacket(conn, 0, payload) }
func writeShellCloseStdin(conn net.Conn) error            { return writeShellPacket(conn, 4, nil) }
func writeShellPacket(conn net.Conn, id byte, payload []byte) error {
	var header [5]byte
	header[0] = id
	binary.LittleEndian.PutUint32(header[1:], uint32(len(payload)))
	if err := writeConnFull(conn, header[:]); err != nil {
		return err
	}
	return writeConnFull(conn, payload)
}
func writeConnFull(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if n > 0 {
			data = data[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func (m *Manager) probeReady(ctx context.Context, s *Stream, authorize func() error) error {
	command := "awk '$4 == \"00010000\" && $5 == \"0001\" && $6 == \"01\" && $8 == \"@" + s.resource.Socket + "\" { print \"READY\"; exit }' /proc/net/unix"
	result, err := m.adb.RunShell(ctx, s.link.TransportID, command, 32, authorize)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return diagnostics.New(fmt.Sprintf("scrcpy readiness probe exited with status %d", result.ExitCode))
	}
	output := strings.TrimSpace(string(result.Stdout))
	if output == "" {
		return errSocketNotListening
	}
	if output != "READY" {
		return diagnostics.New("invalid scrcpy readiness probe response")
	}
	return nil
}

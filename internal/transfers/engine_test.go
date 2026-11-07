package transfers

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func TestWriteShellStdinChunksAndFrames(t *testing.T) {
	data := bytes.Repeat([]byte{0xa5}, shellWriteChunk+1)
	var framed bytes.Buffer
	var written int
	if err := writeShellStdin(&framed, data, func(n int) { written += n }); err != nil {
		t.Fatal(err)
	}
	if written != len(data) {
		t.Fatalf("progress = %d, want %d", written, len(data))
	}
	for _, want := range []int{shellWriteChunk, 1} {
		var header [5]byte
		if _, err := framed.Read(header[:]); err != nil {
			t.Fatal(err)
		}
		if header[0] != shellPacketStdin || int(binary.LittleEndian.Uint32(header[1:])) != want {
			t.Fatalf("frame header = %v, want stdin payload %d", header, want)
		}
		payload := make([]byte, want)
		if _, err := framed.Read(payload); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(payload, data[:want]) {
			t.Fatal("frame payload differs from submitted bytes")
		}
		data = data[want:]
	}
	if framed.Len() != 0 {
		t.Fatalf("unexpected trailing frame bytes: %d", framed.Len())
	}
}
func TestWriteShellCloseStdinUsesEmptyClosePacket(t *testing.T) {
	var framed bytes.Buffer
	if err := writeShellPacket(&framed, shellPacketCloseStdin, nil); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(framed.Bytes(), []byte{shellPacketCloseStdin, 0, 0, 0, 0}) {
		t.Fatalf("CLOSE_STDIN packet = %v", framed.Bytes())
	}
}

func TestReadShellResultCapturesBoundedStreamsAndExit(t *testing.T) {
	var framed bytes.Buffer
	appendPacket := func(id byte, payload []byte) {
		var header [5]byte
		header[0] = id
		binary.LittleEndian.PutUint32(header[1:], uint32(len(payload)))
		framed.Write(header[:])
		framed.Write(payload)
	}
	appendPacket(shellPacketStdout, []byte("ok"))
	appendPacket(shellPacketStderr, []byte("warning"))
	appendPacket(shellPacketExit, []byte{7})
	result, err := readShellResult(&framed, 9)
	if err != nil {
		t.Fatal(err)
	}
	if result.Stdout != "ok" || result.Stderr != "warning" || result.ExitCode != 7 {
		t.Fatalf("unexpected shell result: %#v", result)
	}

	framed.Reset()
	appendPacket(shellPacketStdout, []byte("too long"))
	appendPacket(shellPacketExit, []byte{0})
	if _, err := readShellResult(&framed, 7); err == nil {
		t.Fatal("expected configured output limit error")
	}
}

func TestReadShellResultRejectsInvalidExitPacket(t *testing.T) {
	var framed bytes.Buffer
	framed.Write([]byte{shellPacketExit, 0, 0, 0, 0})
	if _, err := readShellResult(&framed, 10); err == nil {
		t.Fatal("expected malformed exit packet error")
	}
}
func TestIncompleteShellExitPreservesKnownFailure(t *testing.T) {
	result, err := incompleteShellResult(commandResult{ExitCode: 5}, nil, "input incomplete")
	if err != nil || result.ExitCode != 5 {
		t.Fatalf("nonzero early EXIT = (%#v, %v), want known failure", result, err)
	}
	if _, err := incompleteShellResult(commandResult{ExitCode: 0}, nil, "input incomplete"); err == nil {
		t.Fatal("zero early EXIT must remain uncertain")
	}
	readErr := errors.New("malformed stream")
	if _, err := incompleteShellResult(commandResult{ExitCode: 5}, readErr, "input incomplete"); !errors.Is(err, readErr) {
		t.Fatalf("malformed stream error = %v, want %v", err, readErr)
	}
}

func TestReadShellResultSurfacesMalformedPacket(t *testing.T) {
	_, err := readShellResult(bytes.NewReader([]byte{shellPacketStdout}), 10)
	if err == nil {
		t.Fatal("expected truncated packet error")
	}
}

package scrcpy

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"testing"
)

func TestReadPreambleAndDummy(t *testing.T) {
	wire := make([]byte, 68)
	copy(wire, "Pixel 9")
	binary.BigEndian.PutUint32(wire[64:], CodecH264)
	m, err := ReadPreamble(bytes.NewReader(wire))
	if err != nil || m.DeviceName != "Pixel 9" || m.Codec != 0x68323634 {
		t.Fatalf("ReadPreamble() = %+v, %v", m, err)
	}
	if err := ReadDummy(bytes.NewReader([]byte{0})); err != nil {
		t.Fatalf("ReadDummy() = %v", err)
	}
	if err := ReadDummy(bytes.NewReader([]byte{1})); err == nil {
		t.Fatal("ReadDummy accepted nonzero marker")
	}
	binary.BigEndian.PutUint32(wire[64:], 0)
	if _, err := ReadPreamble(bytes.NewReader(wire)); err == nil {
		t.Fatal("ReadPreamble accepted non-H.264 codec")
	}
}

func TestReadPacketSessionRecordsAndMediaFlags(t *testing.T) {
	// v5.0.1 session metadata is three u32 fields; client resize is bit 0.
	var session [12]byte
	binary.BigEndian.PutUint32(session[0:], 0x80000001)
	binary.BigEndian.PutUint32(session[4:], 1080)
	binary.BigEndian.PutUint32(session[8:], 2400)
	p, err := ReadPacket(bytes.NewReader(session[:]), nil, 1024)
	if err != nil || !p.Session || !p.ClientResized || p.Width != 1080 || p.Height != 2400 || len(p.Data) != 0 {
		t.Fatalf("session packet = %+v, %v", p, err)
	}
	// Repeated session records are valid and dimensions are not guessed from media.
	binary.BigEndian.PutUint32(session[0:], 0x80000000)
	binary.BigEndian.PutUint32(session[4:], 720)
	binary.BigEndian.PutUint32(session[8:], 1280)
	p, err = ReadPacket(bytes.NewReader(session[:]), nil, 1024)
	if err != nil || !p.Session || p.ClientResized || p.Width != 720 || p.Height != 1280 {
		t.Fatalf("second session packet = %+v, %v", p, err)
	}

	// Media flags: CONFIG is bit 62 and KEY_FRAME is bit 61; PTS is lower 61 bits.
	media := make([]byte, 15)
	binary.BigEndian.PutUint64(media, 1<<61|123456)
	binary.BigEndian.PutUint32(media[8:], 3)
	copy(media[12:], []byte{0, 0, 1})
	scratch := make([]byte, 0, 4)
	p, err = ReadPacket(bytes.NewReader(media), scratch, 3)
	if err != nil || p.Session || p.Config || !p.KeyFrame || p.PTS != 123456 || !bytes.Equal(p.Data, []byte{0, 0, 1}) || cap(p.Data) != cap(scratch) {
		t.Fatalf("keyframe packet = %+v, %v", p, err)
	}
	binary.BigEndian.PutUint64(media, 1<<62)
	p, err = ReadPacket(bytes.NewReader(media), nil, 3)
	if err != nil || !p.Config || p.KeyFrame || p.PTS != 0 {
		t.Fatalf("config packet = %+v, %v", p, err)
	}
	binary.BigEndian.PutUint32(media, 0x80000002)
	if _, err = ReadPacket(bytes.NewReader(media), nil, 3); err == nil {
		t.Fatal("session record with reserved flag accepted")
	}
	binary.BigEndian.PutUint64(media, 0)
	binary.BigEndian.PutUint32(media[8:], 4)
	if _, err = ReadPacket(bytes.NewReader(media), nil, 3); err == nil {
		t.Fatal("oversized media packet accepted")
	}
	binary.BigEndian.PutUint32(media[8:], 3)
	if _, err = ReadPacket(bytes.NewReader(media[:14]), nil, 3); err == nil {
		t.Fatal("truncated payload accepted")
	}
}

func TestAppendControlGoldenVectorsAndLimits(t *testing.T) {
	base := make([]byte, 1, 32)
	base[0] = 0xaa
	out, err := AppendControl(base, ControlMessage{Type: ControlInjectKeycode, Action: 1, Keycode: 0x1234, Repeat: 2, MetaState: 3})
	want := []byte{0xaa, 0, 1, 0, 0, 0x12, 0x34, 0, 0, 0, 2, 0, 0, 0, 3}
	if err != nil || !bytes.Equal(out, want) || cap(out) != cap(base) {
		t.Fatalf("keycode = %x, %v; want %x", out, err, want)
	}
	out, err = AppendControl(nil, ControlMessage{Type: ControlInjectTouchEvent, Action: 2, PointerID: -1,
		Position: Position{X: 10, Y: 20, Width: 100, Height: 200}, Pressure: 1, ActionButton: 4, Buttons: 5})
	if err != nil || len(out) != 32 || binary.BigEndian.Uint64(out[2:10]) != ^uint64(0) || binary.BigEndian.Uint16(out[22:24]) != 0xffff {
		t.Fatalf("touch vector = %x, %v", out, err)
	}
	out, err = AppendControl(nil, ControlMessage{Type: ControlInjectScrollEvent,
		Position: Position{Width: 1, Height: 1}, HScroll: 16, VScroll: -16})
	if err != nil || int16(binary.BigEndian.Uint16(out[13:15])) != 32767 || int16(binary.BigEndian.Uint16(out[15:17])) != -32768 {
		t.Fatalf("scroll endpoint vector = %x, %v", out, err)
	}
	out, err = AppendControl(nil, ControlMessage{Type: ControlSetClipboard, Sequence: 9, Text: "ok", Paste: true})
	if err != nil || !bytes.Equal(out, []byte{9, 0, 0, 0, 0, 0, 0, 0, 9, 1, 0, 0, 0, 2, 'o', 'k'}) {
		t.Fatalf("clipboard vector = %x, %v", out, err)
	}
	for _, m := range []ControlMessage{
		{Type: ControlInjectText, Text: string(bytes.Repeat([]byte{'x'}, maxInjectText+1))},
		{Type: ControlSetClipboard, Text: string(bytes.Repeat([]byte{'x'}, maxClipboardText+1))},
		{Type: ControlInjectTouchEvent, Pressure: float32(math.NaN()), Position: Position{Width: 1, Height: 1}},
		{Type: ControlInjectScrollEvent, HScroll: 17, Position: Position{Width: 1, Height: 1}},
		{Type: 12},
	} {
		if _, err := AppendControl(nil, m); err == nil {
			t.Fatalf("AppendControl accepted invalid message %+v", m)
		}
	}
	if _, err := AppendControl(nil, ControlMessage{Type: ControlInjectTouchEvent, Pressure: float32(math.Inf(1)), Position: Position{Width: 1, Height: 1}}); err == nil {
		t.Fatal("accepted infinite pressure")
	}
	if _, err := AppendControl(nil, ControlMessage{Type: ControlInjectScrollEvent, VScroll: float32(math.Inf(-1)), Position: Position{Width: 1, Height: 1}}); err == nil {
		t.Fatal("accepted infinite scroll")
	}
}

func TestReadDeviceMessages(t *testing.T) {
	wire := []byte{0, 0, 0, 0, 3, 'a', 'b', 'c'}
	msg, err := ReadDeviceMessage(bytes.NewReader(wire), make([]byte, 3))
	if err != nil || msg.Type != DeviceClipboard || msg.Text != "abc" {
		t.Fatalf("clipboard message = %+v, %v", msg, err)
	}
	wire = []byte{1, 0, 0, 0, 0, 0, 0, 0, 42}
	msg, err = ReadDeviceMessage(bytes.NewReader(wire), nil)
	if err != nil || msg.Type != DeviceClipboardAck || msg.Sequence != 42 {
		t.Fatalf("clipboard ack = %+v, %v", msg, err)
	}
	if _, err := ReadDeviceMessage(bytes.NewReader([]byte{2}), nil); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unexpected device type error = %v", err)
	}
	if _, err := ReadDeviceMessage(bytes.NewReader([]byte{0, 0, 4, 0, 0}), nil); err == nil {
		t.Fatal("oversized clipboard accepted")
	}
	if _, err := ReadDeviceMessage(bytes.NewReader([]byte{0, 0, 0, 0, 1, 0xff}), nil); err == nil {
		t.Fatal("invalid UTF-8 clipboard accepted")
	}
}

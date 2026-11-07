package scrcpy

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"unicode/utf8"
)

const Version = "5.0.1"

const (
	CodecH264          uint32 = 0x68323634
	maxControlMessage         = 1 << 18
	maxInjectText             = 300
	maxClipboardText          = maxControlMessage - 14
	maxDeviceClipboard        = maxControlMessage - 5
)

var (
	ErrProtocol    = errors.New("scrcpy protocol error")
	ErrUnsupported = errors.New("unsupported scrcpy message")
)

type Metadata struct {
	DeviceName string
	Codec      uint32
}

// ReadDummy consumes the forward-tunnel connection marker before other socket setup.
func ReadDummy(r io.Reader) error {
	var b [1]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return fmt.Errorf("scrcpy dummy byte: %w", err)
	}
	if b[0] != 0 {
		return fmt.Errorf("%w: invalid dummy byte %#x", ErrProtocol, b[0])
	}
	return nil
}

// ReadPreamble reads the fixed 64-byte device-name field and the big-endian codec ID.
func ReadPreamble(r io.Reader) (Metadata, error) {
	var b [68]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return Metadata{}, fmt.Errorf("scrcpy preamble: %w", err)
	}
	name := b[:64]
	for i, c := range name {
		if c == 0 {
			name = name[:i]
			break
		}
	}
	if !utf8.Valid(name) {
		return Metadata{}, fmt.Errorf("%w: invalid device name UTF-8", ErrProtocol)
	}
	m := Metadata{DeviceName: string(name), Codec: binary.BigEndian.Uint32(b[64:])}
	if m.Codec != CodecH264 {
		return Metadata{}, fmt.Errorf("%w: video codec %#x is not H.264", ErrProtocol, m.Codec)
	}
	return m, nil
}

type Packet struct {
	Session          bool
	ClientResized    bool
	Width, Height    uint32
	Config, KeyFrame bool
	PTS              uint64
	Data             []byte
}

const (
	flagSession  uint64 = 1 << 63
	flagConfig   uint64 = 1 << 62
	flagKeyFrame uint64 = 1 << 61
	ptsMask      uint64 = flagKeyFrame - 1
)

// ReadPacket reads one v5.0.1 frame. Data aliases scratch when its capacity is sufficient.
func ReadPacket(r io.Reader, scratch []byte, maxBytes uint32) (Packet, error) {
	var h [12]byte
	if _, err := io.ReadFull(r, h[:4]); err != nil {
		return Packet{}, fmt.Errorf("scrcpy packet header: %w", err)
	}
	first := binary.BigEndian.Uint32(h[:4])
	if first&0x80000000 != 0 {
		if first&^uint32(0x80000001) != 0 {
			return Packet{}, fmt.Errorf("%w: reserved session flags set", ErrProtocol)
		}
		if _, err := io.ReadFull(r, h[4:]); err != nil {
			return Packet{}, fmt.Errorf("scrcpy session dimensions: %w", err)
		}
		w, hgt := binary.BigEndian.Uint32(h[4:8]), binary.BigEndian.Uint32(h[8:12])
		if w == 0 || hgt == 0 {
			return Packet{}, fmt.Errorf("%w: zero session dimensions", ErrProtocol)
		}
		return Packet{Session: true, ClientResized: first&1 != 0, Width: w, Height: hgt}, nil
	}
	if _, err := io.ReadFull(r, h[4:]); err != nil {
		return Packet{}, fmt.Errorf("scrcpy packet header: %w", err)
	}
	flags := uint64(first)<<32 | uint64(binary.BigEndian.Uint32(h[4:8]))
	size := binary.BigEndian.Uint32(h[8:])
	if size > maxBytes {
		return Packet{}, fmt.Errorf("%w: packet size %d exceeds limit %d", ErrProtocol, size, maxBytes)
	}
	config, key := flags&flagConfig != 0, flags&flagKeyFrame != 0
	if config && key {
		return Packet{}, fmt.Errorf("%w: config packet marked key frame", ErrProtocol)
	}
	if config && flags&ptsMask != 0 {
		return Packet{}, fmt.Errorf("%w: config packet has PTS", ErrProtocol)
	}
	var data []byte
	if cap(scratch) >= int(size) {
		data = scratch[:int(size)]
	} else {
		data = make([]byte, int(size))
	}
	if _, err := io.ReadFull(r, data); err != nil {
		return Packet{}, fmt.Errorf("scrcpy packet payload: %w", err)
	}
	return Packet{Config: config, KeyFrame: key, PTS: flags & ptsMask, Data: data}, nil
}

type ControlType uint8

const (
	ControlInjectKeycode ControlType = iota
	ControlInjectText
	ControlInjectTouchEvent
	ControlInjectScrollEvent
	ControlBackOrScreenOn
	ControlExpandNotificationPanel
	ControlExpandSettingsPanel
	ControlCollapsePanels
	ControlGetClipboard
	ControlSetClipboard
	ControlSetDisplayPower
	ControlRotateDevice
	ControlResetVideo ControlType = 17
)

type Position struct {
	X, Y          int32
	Width, Height uint16
}

// ControlMessage is the exposed v5.0.1 SDK-control union (UHID and unrelated types omitted).
type ControlMessage struct {
	Type                       ControlType
	Action                     uint8
	Keycode, Repeat, MetaState int32
	Text                       string
	PointerID                  int64
	Position                   Position
	// Pressure is normalized [0,1], encoded as unsigned 16-bit fixed point (2^16 scale).
	Pressure              float32
	ActionButton, Buttons int32
	// HScroll and VScroll use Android scroll units [-16,16], encoded as signed
	// 16-bit fixed point after dividing by 16 (2^15 scale; +16 maps to 32767).
	HScroll, VScroll float32
	CopyKey          uint8
	Sequence         uint64
	Paste            bool
	On               bool
}

// AppendControl encodes one control message, reusing dst's capacity where possible.
func AppendControl(dst []byte, m ControlMessage) ([]byte, error) {
	start := len(dst)
	put8 := func(v byte) { dst = append(dst, v) }
	put16 := func(v uint16) { dst = binary.BigEndian.AppendUint16(dst, v) }
	put32 := func(v uint32) { dst = binary.BigEndian.AppendUint32(dst, v) }
	put64 := func(v uint64) { dst = binary.BigEndian.AppendUint64(dst, v) }
	putString := func(s string, max int) error {
		if !utf8.ValidString(s) {
			return fmt.Errorf("invalid UTF-8 string")
		}
		if len(s) > max {
			return fmt.Errorf("string length %d exceeds %d", len(s), max)
		}
		dst = binary.BigEndian.AppendUint32(dst, uint32(len(s)))
		dst = append(dst, s...)
		return nil
	}
	putPosition := func(p Position) error {
		if p.Width == 0 || p.Height == 0 {
			return fmt.Errorf("position dimensions must be nonzero")
		}
		put32(uint32(p.X))
		put32(uint32(p.Y))
		put16(p.Width)
		put16(p.Height)
		return nil
	}
	put8(byte(m.Type))
	bad := func(err error) ([]byte, error) { return dst[:start], err }
	switch m.Type {
	case ControlInjectKeycode:
		put8(m.Action)
		put32(uint32(m.Keycode))
		put32(uint32(m.Repeat))
		put32(uint32(m.MetaState))
	case ControlInjectText:
		if len(m.Text) > maxInjectText {
			return bad(fmt.Errorf("text exceeds %d bytes", maxInjectText))
		}
		if err := putString(m.Text, maxInjectText); err != nil {
			return bad(err)
		}
	case ControlInjectTouchEvent:
		if math.IsNaN(float64(m.Pressure)) || m.Pressure < 0 || m.Pressure > 1 {
			return bad(fmt.Errorf("pressure outside [0,1]"))
		}
		put8(m.Action)
		put64(uint64(m.PointerID))
		if err := putPosition(m.Position); err != nil {
			return bad(err)
		}
		// Matches sc_float_to_u16fp(): truncate at 2^16, saturating only
		// the valid 1.0 endpoint to 0xffff.
		p := uint32(m.Pressure * 65536)
		if p >= 0xffff {
			p = 0xffff
		}
		put16(uint16(p))
		put32(uint32(m.ActionButton))
		put32(uint32(m.Buttons))
	case ControlInjectScrollEvent:
		if math.IsNaN(float64(m.HScroll)) || math.IsNaN(float64(m.VScroll)) || m.HScroll < -16 || m.HScroll > 16 || m.VScroll < -16 || m.VScroll > 16 {
			return bad(fmt.Errorf("scroll outside [-16,16]"))
		}
		if err := putPosition(m.Position); err != nil {
			return bad(err)
		}
		// Matches sc_float_to_i16fp(): truncate after normalization; +1.0
		// saturates to 0x7fff and -1.0 maps to -0x8000.
		scroll := func(v float32) int16 {
			n := int32((v / 16) * 32768)
			if n >= 0x7fff {
				n = 0x7fff
			}
			return int16(n)
		}
		put16(uint16(scroll(m.HScroll)))
		put16(uint16(scroll(m.VScroll)))
		put32(uint32(m.Buttons))
	case ControlBackOrScreenOn:
		put8(m.Action)
	case ControlExpandNotificationPanel, ControlExpandSettingsPanel, ControlCollapsePanels, ControlRotateDevice, ControlResetVideo:
	case ControlGetClipboard:
		if m.CopyKey > 2 {
			return bad(fmt.Errorf("invalid clipboard copy key"))
		}
		put8(m.CopyKey)
	case ControlSetClipboard:
		if len(m.Text) > maxClipboardText {
			return bad(fmt.Errorf("clipboard text exceeds %d bytes", maxClipboardText))
		}
		put64(m.Sequence)
		if m.Paste {
			put8(1)
		} else {
			put8(0)
		}
		if err := putString(m.Text, maxClipboardText); err != nil {
			return bad(err)
		}
	case ControlSetDisplayPower:
		if m.On {
			put8(1)
		} else {
			put8(0)
		}
	default:
		return bad(fmt.Errorf("%w: control type %d", ErrUnsupported, m.Type))
	}
	if len(dst)-start > maxControlMessage {
		return dst[:start], fmt.Errorf("control message exceeds %d bytes", maxControlMessage)
	}
	return dst, nil
}

type DeviceMessageType uint8

const (
	DeviceClipboard DeviceMessageType = iota
	DeviceClipboardAck
)

type DeviceMessage struct {
	Type     DeviceMessageType
	Text     string
	Sequence uint64
}

// ReadDeviceMessage reads clipboard text or clipboard acknowledgement. Text is owned by the result.
func ReadDeviceMessage(r io.Reader, scratch []byte) (DeviceMessage, error) {
	var t [1]byte
	if _, err := io.ReadFull(r, t[:]); err != nil {
		return DeviceMessage{}, fmt.Errorf("scrcpy device message type: %w", err)
	}
	switch DeviceMessageType(t[0]) {
	case DeviceClipboard:
		var n [4]byte
		if _, err := io.ReadFull(r, n[:]); err != nil {
			return DeviceMessage{}, fmt.Errorf("scrcpy clipboard length: %w", err)
		}
		size := binary.BigEndian.Uint32(n[:])
		if size > maxDeviceClipboard {
			return DeviceMessage{}, fmt.Errorf("%w: clipboard length %d exceeds %d", ErrProtocol, size, maxDeviceClipboard)
		}
		b := scratch[:0]
		if cap(scratch) < int(size) {
			b = make([]byte, int(size))
		} else {
			b = scratch[:int(size)]
		}
		if _, err := io.ReadFull(r, b); err != nil {
			return DeviceMessage{}, fmt.Errorf("scrcpy clipboard payload: %w", err)
		}
		if !utf8.Valid(b) {
			return DeviceMessage{}, fmt.Errorf("%w: clipboard text is invalid UTF-8", ErrProtocol)
		}
		return DeviceMessage{Type: DeviceClipboard, Text: string(b)}, nil
	case DeviceClipboardAck:
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return DeviceMessage{}, fmt.Errorf("scrcpy clipboard acknowledgement: %w", err)
		}
		return DeviceMessage{Type: DeviceClipboardAck, Sequence: binary.BigEndian.Uint64(b[:])}, nil
	default:
		return DeviceMessage{}, fmt.Errorf("%w: device message type %d", ErrUnsupported, t[0])
	}
}

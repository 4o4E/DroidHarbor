package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image/png"
	"io"
	"sync"
)

func (m *Manager) OriginalScreenshot(ctx context.Context, owner, sessionID string) (*Screenshot, error) {
	scope, err := m.SessionScope(ctx, owner, sessionID, false)
	if err != nil {
		return nil, err
	}
	slotHeld := false
	defer func() {
		if !slotHeld {
			scope.Close()
		}
	}()
	select {
	case m.screenshotSlots <- struct{}{}:
		slotHeld = true
	case <-scope.Context.Done():
		return nil, scope.Context.Err()
	}
	failed := func(err error) (*Screenshot, error) {
		<-m.screenshotSlots
		slotHeld = false
		return nil, err
	}
	if err := scope.Check(); err != nil {
		return failed(err)
	}
	opCtx, cancelOp := context.WithTimeout(scope.Context, screenshotTimeout)
	conn, err := m.deps.ADB.Open(opCtx, scope.Link.TransportID, "exec:screencap -p", scope.Check)
	if err != nil {
		operationErr := opCtx.Err()
		cancelOp()
		if scope.Context.Err() != nil {
			return failed(scope.Context.Err())
		}
		if operationErr != nil {
			return failed(operationErr)
		}
		return failed(&Error{Status: 503, Code: "SCREENSHOT_FAILED", Message: "无法获取设备原始截图", Cause: err})
	}
	data, readErr := io.ReadAll(io.LimitReader(conn, maxScreenshotBytes+1))
	closeErr := conn.Close()
	operationErr := opCtx.Err()
	cancelOp()
	if readErr != nil || closeErr != nil {
		if scope.Context.Err() != nil {
			return failed(scope.Context.Err())
		}
		if operationErr != nil {
			return failed(operationErr)
		}
		return failed(&Error{Status: 503, Code: "SCREENSHOT_FAILED", Message: "设备原始截图读取失败", Cause: errors.Join(readErr, closeErr)})
	}
	if len(data) == 0 || len(data) > maxScreenshotBytes {
		return failed(problem(422, "INVALID_SCREENSHOT", "设备原始截图为空或超过大小限制"))
	}
	if err := scope.Check(); err != nil {
		return failed(err)
	}
	config, err := png.DecodeConfig(bytes.NewReader(data))
	if err == nil {
		err = validatePNGStructure(data)
	}
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return failed(&Error{Status: 502, Code: "INVALID_SCREENSHOT", Message: "设备未返回完整的有效 PNG 截图", Cause: err})
	}
	if err := scope.Check(); err != nil {
		return failed(err)
	}
	reader := &scopedScreenshotReader{
		scope:  scope,
		reader: bytes.NewReader(data),
		close:  func() { <-m.screenshotSlots; slotHeld = false; scope.Close() },
	}
	// Ownership of the authorization scope and semaphore transfers to Reader.Close.
	slotHeld = true
	return &Screenshot{Reader: reader, ContentType: "image/png", Size: int64(len(data)), Width: config.Width, Height: config.Height}, nil
}

func validatePNGStructure(data []byte) error {
	const signature = "\x89PNG\r\n\x1a\n"
	if len(data) < len(signature) || string(data[:len(signature)]) != signature {
		return errors.New("invalid PNG signature")
	}
	offset := len(signature)
	sawHeader, sawData, sawEnd, sawPalette := false, false, false, false
	endedData := false
	requiresPalette := false
	pngColorType := byte(0)
	chunks := 0
	for offset < len(data) {
		chunks++
		if chunks > 65536 || len(data)-offset < 12 {
			return errors.New("truncated or excessive PNG chunks")
		}
		chunkLength := uint64(binary.BigEndian.Uint32(data[offset : offset+4]))
		if chunkLength > uint64(len(data)-offset-12) {
			return errors.New("truncated PNG chunk")
		}
		typeStart := offset + 4
		dataStart := offset + 8
		crcStart := dataStart + int(chunkLength)
		chunkEnd := crcStart + 4
		if crc32.ChecksumIEEE(data[typeStart:crcStart]) != binary.BigEndian.Uint32(data[crcStart:chunkEnd]) {
			return errors.New("invalid PNG chunk CRC")
		}
		for _, letter := range data[typeStart : typeStart+4] {
			if !((letter >= 'A' && letter <= 'Z') || (letter >= 'a' && letter <= 'z')) {
				return errors.New("invalid PNG chunk type")
			}
		}
		if data[typeStart+2]&0x20 != 0 {
			return errors.New("invalid PNG reserved chunk bit")
		}
		kind := string(data[typeStart : typeStart+4])
		switch kind {
		case "IHDR":
			if sawHeader || chunks != 1 || chunkLength != 13 {
				return errors.New("invalid PNG IHDR")
			}
			sawHeader = true
			pngColorType = data[dataStart+9]
			requiresPalette = pngColorType == 3
		case "PLTE":
			if !sawHeader || sawData || sawPalette || pngColorType == 0 || pngColorType == 4 || chunkLength == 0 || chunkLength > 768 || chunkLength%3 != 0 {
				return errors.New("invalid PNG PLTE")
			}
			sawPalette = true
		case "IDAT":
			if !sawHeader || endedData {
				return errors.New("invalid PNG IDAT ordering")
			}
			sawData = true
		case "IEND":
			if !sawHeader || !sawData || chunkLength != 0 {
				return errors.New("invalid PNG IEND")
			}
			sawEnd = true
		default:
			if !sawHeader {
				return errors.New("PNG does not begin with IHDR")
			}
			if sawData {
				endedData = true
			}
			if data[typeStart]&0x20 == 0 {
				return errors.New("unknown critical PNG chunk")
			}
		}
		offset = chunkEnd
		if sawEnd {
			if offset != len(data) {
				return errors.New("trailing bytes after PNG IEND")
			}
			break
		}
	}
	if !sawHeader || !sawData || !sawEnd || (requiresPalette && !sawPalette) {
		return errors.New("incomplete PNG structure")
	}
	return nil
}

type scopedScreenshotReader struct {
	scope  Scope
	reader *bytes.Reader
	once   sync.Once
	close  func()
}

func (r *scopedScreenshotReader) Read(buffer []byte) (int, error) {
	if err := r.scope.Context.Err(); err != nil {
		return 0, err
	}
	if err := r.scope.Check(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

func (r *scopedScreenshotReader) Close() error {
	r.once.Do(r.close)
	return nil
}

var _ io.ReadCloser = (*scopedScreenshotReader)(nil)

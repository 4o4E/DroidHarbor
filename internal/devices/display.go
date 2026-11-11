package devices

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strconv"
	"time"

	"droidharbor/internal/diagnostics"
)

// Screen and encoder observations are separate: encoding alignment need not
// exactly match Android's logical display. These values reserve layout only.
type DisplayInfo struct {
	Screen *ScreenGeometry `json:"screen,omitempty"`
	Video  *VideoGeometry  `json:"video,omitempty"`
}

type ScreenGeometry struct {
	Width      int       `json:"width"`
	Height     int       `json:"height"`
	Rotation   int       `json:"rotation"`
	ObservedAt time.Time `json:"observedAt"`
}

type VideoGeometry struct {
	Width      int       `json:"width"`
	Height     int       `json:"height"`
	ObservedAt time.Time `json:"observedAt"`
}

var logicalDisplaySize = regexp.MustCompile(`\breal\s+(\d+)\s*x\s*(\d+)\b`)
var logicalDisplayRotation = regexp.MustCompile(`\brotation\s+([0-3])\b`)

// dumpsys display is available on Android versions without cmd display get-displays.
// WindowManager's override supplies the current logical orientation; the base
// describes the physical panel and is used only when the override is null.
func parseScreenGeometry(output io.Reader) (ScreenGeometry, error) {
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 4096), 64<<10)
	primary := false
	var base []byte
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if !primary {
			primary = bytes.Equal(line, []byte("Display 0:"))
			continue
		}
		if bytes.HasPrefix(line, []byte("Display ")) && bytes.HasSuffix(line, []byte(":")) {
			break
		}
		if information, ok := bytes.CutPrefix(line, []byte("mBaseDisplayInfo=")); ok {
			base = bytes.Clone(information)
		}
		if information, ok := bytes.CutPrefix(line, []byte("mOverrideDisplayInfo=")); ok {
			if bytes.Equal(bytes.TrimSpace(information), []byte("null")) {
				return parseDisplayGeometry(base)
			}
			return parseDisplayGeometry(information)
		}
	}
	if err := scanner.Err(); err != nil {
		return ScreenGeometry{}, err
	}
	if !primary {
		return ScreenGeometry{}, diagnostics.New("primary display information missing")
	}
	return ScreenGeometry{}, diagnostics.New("primary display override information missing")
}

func parseDisplayGeometry(information []byte) (ScreenGeometry, error) {
	size, rotation := logicalDisplaySize.FindSubmatch(information), logicalDisplayRotation.FindSubmatch(information)
	if len(size) != 3 || len(rotation) != 2 {
		return ScreenGeometry{}, diagnostics.New("primary display geometry missing")
	}
	width, widthErr := strconv.Atoi(string(size[1]))
	height, heightErr := strconv.Atoi(string(size[2]))
	if widthErr != nil || heightErr != nil || width <= 0 || height <= 0 || width > 65535 || height > 65535 {
		return ScreenGeometry{}, diagnostics.New("primary display dimensions invalid")
	}
	return ScreenGeometry{Width: width, Height: height, Rotation: int(rotation[1][0] - '0'), ObservedAt: time.Now().UTC()}, nil
}

func (r *Registry) readDisplay(ctx context.Context, transportID uint64) (ScreenGeometry, error) {
	probeCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	stream, err := r.client.Open(probeCtx, transportID, "exec:dumpsys display", probeCtx.Err)
	if err == nil {
		geometry, readErr := parseScreenGeometry(io.LimitReader(stream, 256<<10))
		err = errors.Join(readErr, stream.Close())
		if err == nil {
			return geometry, nil
		}
	}
	return ScreenGeometry{}, &Error{Code: "DISPLAY_INFO_UNAVAILABLE", Message: "无法读取设备显示尺寸，请同步设备状态后重试", Status: 503, Cause: err}
}

type displayWriter interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func saveScreenGeometry(ctx context.Context, writer displayWriter, deviceID string, geometry ScreenGeometry) error {
	encoded, err := json.Marshal(geometry)
	if err != nil {
		return err
	}
	_, err = writer.ExecContext(ctx, `UPDATE devices SET display_info=json_set(coalesce(display_info,'{}'),'$.screen',json(?)) WHERE id=?`, string(encoded), deviceID)
	return err
}

// RecordVideoGeometry runs on metadata boundaries, never on every video frame.
// Keep an observation current after a reconnect even when dimensions are equal.
func RecordVideoGeometry(ctx context.Context, db *sql.DB, deviceID string, width, height int) error {
	geometry := VideoGeometry{Width: width, Height: height, ObservedAt: time.Now().UTC()}
	encoded, err := json.Marshal(geometry)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `UPDATE devices SET display_info=json_set(coalesce(display_info,'{}'),'$.video',json(?)) WHERE id=?`, string(encoded), deviceID)
	return err
}

func decodeDisplayInfo(encoded sql.NullString) (*DisplayInfo, error) {
	if !encoded.Valid {
		return nil, nil
	}
	var info DisplayInfo
	if err := json.Unmarshal([]byte(encoded.String), &info); err != nil {
		return nil, errors.New("设备显示信息记录无效")
	}
	return &info, nil
}

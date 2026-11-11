package devices

import (
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

var primaryDisplayLine = regexp.MustCompile(`(?m)^\s*Display id 0:\s*([^\r\n]+)`)
var logicalDisplaySize = regexp.MustCompile(`\breal\s+(\d+)\s*x\s*(\d+)\b`)
var logicalDisplayRotation = regexp.MustCompile(`\brotation\s+([0-3])\b`)

func parseScreenGeometry(output string) (ScreenGeometry, error) {
	line := primaryDisplayLine.FindStringSubmatch(output)
	if len(line) != 2 {
		return ScreenGeometry{}, diagnostics.New("primary display information missing")
	}
	size, rotation := logicalDisplaySize.FindStringSubmatch(line[1]), logicalDisplayRotation.FindStringSubmatch(line[1])
	if len(size) != 3 || len(rotation) != 2 {
		return ScreenGeometry{}, diagnostics.New("primary display geometry missing")
	}
	width, widthErr := strconv.Atoi(size[1])
	height, heightErr := strconv.Atoi(size[2])
	if widthErr != nil || heightErr != nil || width <= 0 || height <= 0 || width > 65535 || height > 65535 {
		return ScreenGeometry{}, diagnostics.New("primary display dimensions invalid")
	}
	return ScreenGeometry{Width: width, Height: height, Rotation: int(rotation[1][0] - '0'), ObservedAt: time.Now().UTC()}, nil
}

func (r *Registry) readDisplay(ctx context.Context, transportID uint64) (ScreenGeometry, error) {
	probeCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	stream, err := r.client.Open(probeCtx, transportID, "exec:cmd display get-displays", probeCtx.Err)
	if err == nil {
		data, readErr := io.ReadAll(io.LimitReader(stream, (64<<10)+1))
		err = errors.Join(readErr, stream.Close())
		if len(data) > 64<<10 {
			err = errors.Join(err, diagnostics.New("display information exceeds size limit"))
		}
		if err == nil {
			var geometry ScreenGeometry
			geometry, err = parseScreenGeometry(string(data))
			if err == nil {
				return geometry, nil
			}
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

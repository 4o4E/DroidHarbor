package capture

import (
	"errors"
	"fmt"
	"strconv"
)

// settings. Values are argv entries, not a shell command.
func serverOptions(scid int32, settings Settings) ([]string, error) {
	if scid <= 0 || scid > 0x7fffffff {
		return nil, errors.New("invalid scrcpy scid")
	}
	if settings.MaxSize <= 0 || settings.MaxFPS <= 0 || settings.BitRate <= 0 {
		return nil, errors.New("invalid capture quality settings")
	}
	// Do not force video_codec_options: Android hardware encoders may reject
	// the profile or I-frame interval (OMX 0x80001001) or produce no frames.
	// The media layer requests RESET_VIDEO when a viewer needs a key frame.
	return []string{
		"scid=" + fmt.Sprintf("%08x", uint32(scid)),
		"video=true", "audio=false", "control=true", "video_codec=h264",
		"max_size=" + strconv.Itoa(settings.MaxSize),
		"max_fps=" + strconv.Itoa(settings.MaxFPS),
		"video_bit_rate=" + strconv.Itoa(settings.BitRate),
		"downsize_on_error=false", "power_on=false", "stay_awake=false",
		"keep_active=false", "show_touches=false", "clipboard_autosync=false",
		"cleanup=true", "send_dummy_byte=true", "send_device_meta=true",
		"send_stream_meta=true", "send_frame_meta=true", "tunnel_forward=true",
	}, nil
}

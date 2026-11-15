package media

import (
	"fmt"
	"testing"
)

func TestDeviceVolumeReportedRange(t *testing.T) {
	for _, current := range []int{0, 19, 25} {
		input := []byte(fmt.Sprintf("[V] will control stream=3 (STREAM_MUSIC)\n[V] volume is %d in range [0..25]\r\n", current))
		volume, err := parseDeviceVolume(input)
		if err != nil || volume.Current != current || volume.Min != 0 || volume.Max != 25 {
			t.Fatalf("current=%d parsed=%+v error=%v", current, volume, err)
		}
	}
}

func TestDeviceVolumeRejectsUnknownAndInvalidReadings(t *testing.T) {
	for _, output := range []string{
		"[E] unavailable", "[V] volume is 26 in range [0..25]\n",
		"[V] volume is 2 in range [3..25]\n", "[V] volume is 0 in range [0..0]\n",
		"[V] volume is 999999999999999999999 in range [0..25]\n",
	} {
		if _, err := parseDeviceVolume([]byte(output)); err == nil {
			t.Fatalf("accepted unknown/invalid device reading %q", output)
		}
	}
}

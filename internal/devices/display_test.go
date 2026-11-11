package devices

import "testing"

func TestScreenGeometryUsesPrimaryDisplayAndCurrentRotation(t *testing.T) {
	for _, tc := range []struct {
		name, output            string
		width, height, rotation int
	}{
		{"android13-portrait", "Displays:\n  Display id 0: DisplayInfo{\"内置屏幕\", real 1080 x 2400, rotation 0, state OFF}\n", 1080, 2400, 0},
		{"android16-landscape", "  Display id 7: DisplayInfo{real 640 x 480, rotation 0}\n  Display id 0: DisplayInfo{real 2560 x 1600, rotation 1, state ON}\n", 2560, 1600, 1},
		{"reverse-portrait", "Display id 0: DisplayInfo{real 1080 x 2400, rotation 2}", 1080, 2400, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			geometry, err := parseScreenGeometry(tc.output)
			if err != nil || geometry.Width != tc.width || geometry.Height != tc.height || geometry.Rotation != tc.rotation || geometry.ObservedAt.IsZero() {
				t.Fatalf("geometry=%+v err=%v", geometry, err)
			}
		})
	}
}

func TestScreenGeometryRejectsUnknownInsteadOfInventingSize(t *testing.T) {
	for _, output := range []string{
		"Display id 1: DisplayInfo{real 1920 x 1080, rotation 0}",
		"Display id 0: DisplayInfo{real 0 x 2400, rotation 0}",
		"Display id 0: DisplayInfo{real 1080 x 999999, rotation 0}",
		"Display id 0: DisplayInfo{real 1080 x 2400, rotation 4}",
		"Display id 0: DisplayInfo{real 1080 x 2400}\nDisplay id 1: DisplayInfo{rotation 0}",
	} {
		if _, err := parseScreenGeometry(output); err == nil {
			t.Fatalf("accepted unavailable or invalid primary display: %q", output)
		}
	}
}

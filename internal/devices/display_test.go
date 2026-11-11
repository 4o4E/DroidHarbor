package devices

import (
	"strings"
	"testing"
)

func TestScreenGeometryUsesPrimaryDisplayAndCurrentRotation(t *testing.T) {
	for _, tc := range []struct {
		name, output            string
		width, height, rotation int
	}{
		{"android13-portrait", "Logical Displays:\n  Display 0:\n    mBaseDisplayInfo=DisplayInfo{real 1080 x 2400, rotation 0, state OFF}\n    mOverrideDisplayInfo=DisplayInfo{real 1080 x 2400, rotation 0, state ON}\n", 1080, 2400, 0},
		{"android16-landscape", "  Display 7:\n    mBaseDisplayInfo=DisplayInfo{real 640 x 480, rotation 0}\n  Display 0:\n    mBaseDisplayInfo=DisplayInfo{real 1600 x 2560, rotation 0}\n    mOverrideDisplayInfo=DisplayInfo{real 2560 x 1600, rotation 1}\n", 2560, 1600, 1},
		{"base-without-window-override", "  Display 0:\n    mBaseDisplayInfo=DisplayInfo{real 1080 x 2400, rotation 0}\n    mOverrideDisplayInfo=null\n", 1080, 2400, 0},
		{"reverse-portrait", "  Display 0:\n    mBaseDisplayInfo=DisplayInfo{real 1080 x 2400, rotation 0}\n    mOverrideDisplayInfo=DisplayInfo{real 1080 x 2400, rotation 2}\n", 1080, 2400, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			geometry, err := parseScreenGeometry(strings.NewReader(tc.output))
			if err != nil || geometry.Width != tc.width || geometry.Height != tc.height || geometry.Rotation != tc.rotation || geometry.ObservedAt.IsZero() {
				t.Fatalf("geometry=%+v err=%v", geometry, err)
			}
		})
	}
}

func TestScreenGeometryRejectsUnknownInsteadOfInventingSize(t *testing.T) {
	for _, output := range []string{
		"Unknown command: get-displays",
		"Display 1:\n mBaseDisplayInfo=DisplayInfo{real 1920 x 1080, rotation 0}",
		"Display 0:\n mBaseDisplayInfo=DisplayInfo{real 0 x 2400, rotation 0}",
		"Display 0:\n mBaseDisplayInfo=DisplayInfo{real 1080 x 999999, rotation 0}",
		"Display 0:\n mBaseDisplayInfo=DisplayInfo{real 1080 x 2400, rotation 4}",
		"Display 0:\n mBaseDisplayInfo=DisplayInfo{real 1080 x 2400}\nDisplay 1:\n mOverrideDisplayInfo=DisplayInfo{real 1920 x 1080, rotation 0}",
		"Display 0:\n mBaseDisplayInfo=DisplayInfo{real 1080 x 2400, rotation 0}\n mOverrideDisplayInfo=DisplayInfo{real 0 x 2400, rotation 0}",
		"Display 0:\n mBaseDisplayInfo=DisplayInfo{real 1600 x 2560, rotation 0}",
	} {
		if _, err := parseScreenGeometry(strings.NewReader(output)); err == nil {
			t.Fatalf("accepted unavailable or invalid primary display: %q", output)
		}
	}
}

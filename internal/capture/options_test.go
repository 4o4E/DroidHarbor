package capture

import "testing"

func TestServerOptionsRejectsInvalidInputs(t *testing.T) {
	for _, tc := range []struct {
		scid     int32
		settings Settings
	}{
		{0, Settings{MaxSize: 1, MaxFPS: 1, BitRate: 1}},
		{1, Settings{MaxSize: 0, MaxFPS: 1, BitRate: 1}},
		{1, Settings{MaxSize: 1, MaxFPS: 1, BitRate: 0}},
	} {
		if _, err := serverOptions(tc.scid, tc.settings); err == nil {
			t.Fatalf("serverOptions(%d, %+v) succeeded", tc.scid, tc.settings)
		}
	}
}

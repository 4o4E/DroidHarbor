package media

import "testing"

func TestOpusPacketSamples(t *testing.T) {
	for _, tc := range []struct {
		name   string
		packet []byte
		want   uint32
	}{
		{"CELT 2.5ms", []byte{0x80}, 120},
		{"CELT 5ms", []byte{0x88}, 240},
		{"CELT 10ms", []byte{0x90}, 480},
		{"CELT 20ms", []byte{0x98}, 960},
		{"stereo CELT", []byte{0x9c}, 960},
		{"hybrid 10ms", []byte{0x60}, 480},
		{"hybrid 20ms", []byte{0x68}, 960},
		{"SILK 10ms", []byte{0x00}, 480},
		{"SILK 20ms", []byte{0x08}, 960},
		{"SILK 40ms", []byte{0x10}, 1920},
		{"SILK 60ms", []byte{0x18}, 2880},
		{"two CBR frames", []byte{0x19}, 5760},
		{"two VBR frames", []byte{0x9a}, 1920},
		{"six frames", []byte{0x9b, 6}, 5760},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := opusPacketSamples(tc.packet)
			if err != nil || got != tc.want {
				t.Fatalf("samples=%d, error=%v; want %d", got, err, tc.want)
			}
		})
	}
}

func TestOpusPacketSamplesRejectsInvalidDuration(t *testing.T) {
	for _, packet := range [][]byte{nil, {0x9b}, {0x9b, 0}, {0x9b, 7}, {0x1b, 3}} {
		if _, err := opusPacketSamples(packet); err == nil {
			t.Fatalf("accepted invalid Opus duration: %x", packet)
		}
	}
}

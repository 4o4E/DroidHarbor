package media

import "errors"

// RTP time advances by encoded samples, not capture wall-clock jitter.
// Durations and frame counts are defined by RFC 6716 section 3.1.
func opusPacketSamples(packet []byte) (uint32, error) {
	if len(packet) == 0 {
		return 0, errors.New("empty Opus packet")
	}
	configuration := packet[0] >> 3
	var samples uint32
	switch {
	case configuration >= 16:
		samples = 120 << (configuration & 3)
	case configuration >= 12:
		samples = 480 << (configuration & 1)
	case configuration&3 == 3:
		samples = 2880
	default:
		samples = 480 << (configuration & 3)
	}
	frames := uint32(1)
	switch packet[0] & 3 {
	case 1, 2:
		frames = 2
	case 3:
		if len(packet) < 2 {
			return 0, errors.New("missing Opus frame count")
		}
		frames = uint32(packet[1] & 63)
	}
	samples *= frames
	if frames == 0 || samples > 5760 {
		return 0, errors.New("Opus packet duration must be within 120 ms")
	}
	return samples, nil
}

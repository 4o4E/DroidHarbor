package capture

import (
	"errors"
	"strconv"
	"strings"
)

// processStartTicks extracts field 22 from /proc/<pid>/stat. The comm field is
// parenthesized but may itself contain spaces or ')', so parsing starts after
// its final closing parenthesis rather than splitting the complete line.
func processStartTicks(stat string) (uint64, error) {
	close := strings.LastIndexByte(stat, ')')
	if close < 0 || close+1 >= len(stat) || stat[close+1] != ' ' {
		return 0, errors.New("invalid process stat")
	}
	fields := strings.Fields(stat[close+2:])
	// The first token after comm is field 3; starttime is field 22.
	if len(fields) <= 19 {
		return 0, errors.New("incomplete process stat")
	}
	value, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || value == 0 {
		return 0, errors.New("invalid process start ticks")
	}
	return value, nil
}

func validBootUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for i, r := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

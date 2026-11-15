package diagnostics

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
)

var logZone = time.FixedZone("UTC+8", 8*60*60)
var logBuffers = sync.Pool{New: func() any { b := make([]byte, 0, 1024); return &b }}

// The device column is display-only. Protocols, persistence and authorization
// keep the complete ID; request/session IDs in the details remain complete.
type patternHandler struct {
	output io.Writer
	level  slog.Level
	mu     *sync.Mutex
	attrs  []slog.Attr
	groups []string
}

func newPatternHandler(output io.Writer, level slog.Level) *patternHandler {
	return &patternHandler{output: output, level: level, mu: new(sync.Mutex)}
}

func (h *patternHandler) Enabled(_ context.Context, level slog.Level) bool { return level >= h.level }

func (h *patternHandler) Handle(_ context.Context, record slog.Record) error {
	pooled := logBuffers.Get().(*[]byte)
	buffer := record.Time.In(logZone).AppendFormat((*pooled)[:0], "2006-01-02 15:04:05.000")
	buffer = append(buffer, ' ')
	level := record.Level.String()
	buffer = append(buffer, level...)
	for i := len(level); i < 5; i++ {
		buffer = append(buffer, ' ')
	}
	buffer = append(buffer, ' ', '[')
	deviceAt := len(buffer)
	buffer = append(buffer, "--------] "...)
	buffer = appendLogText(buffer, record.Message)
	for _, attr := range h.attrs {
		buffer = appendPatternAttr(buffer, attr, deviceAt, " | ")
	}
	groupAt := len(buffer)
	if record.NumAttrs() > 0 {
		for range h.groups {
			buffer = append(buffer, " | ("...)
		}
	}
	payloadAt := len(buffer)
	record.Attrs(func(attr slog.Attr) bool {
		separator := " | "
		if len(h.groups) > 0 && len(buffer) == payloadAt {
			separator = ""
		}
		buffer = appendPatternAttr(buffer, attr, deviceAt, separator)
		return true
	})
	if record.NumAttrs() > 0 {
		if len(buffer) == groupAt+len(h.groups)*4 {
			buffer = buffer[:groupAt]
		} else {
			for range h.groups {
				buffer = append(buffer, ')')
			}
		}
	}
	buffer = append(buffer, '\n')
	h.mu.Lock()
	n, err := h.output.Write(buffer)
	h.mu.Unlock()
	if err == nil && n != len(buffer) {
		err = io.ErrShortWrite
	}
	if cap(buffer) <= 64<<10 {
		*pooled = buffer[:0]
		logBuffers.Put(pooled)
	}
	return err
}

func (h *patternHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	resolved := resolvePatternAttrs(attrs)
	for i := len(h.groups) - 1; i >= 0; i-- {
		resolved = []slog.Attr{{Key: h.groups[i], Value: slog.GroupValue(resolved...)}}
	}
	clone := *h
	clone.attrs = make([]slog.Attr, 0, len(h.attrs)+len(resolved))
	clone.attrs = append(clone.attrs, h.attrs...)
	clone.attrs = append(clone.attrs, resolved...)
	return &clone
}

func (h *patternHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	clone := *h
	clone.groups = append(append([]string(nil), h.groups...), name)
	return &clone
}

func resolvePatternAttrs(attrs []slog.Attr) []slog.Attr {
	resolved := make([]slog.Attr, len(attrs))
	for i, attr := range attrs {
		attr.Value = attr.Value.Resolve()
		if attr.Value.Kind() == slog.KindGroup {
			attr.Value = slog.GroupValue(resolvePatternAttrs(attr.Value.Group())...)
		}
		resolved[i] = attr
	}
	return resolved
}

func appendPatternAttr(buffer []byte, attr slog.Attr, deviceAt int, separator string) []byte {
	value := attr.Value.Resolve()
	if attr.Key == "deviceId" && value.Kind() == slog.KindString {
		id := value.String()
		for i := range 8 {
			buffer[deviceAt+i] = '-'
		}
		copy(buffer[deviceAt:deviceAt+8], id)
		return buffer
	}
	if attr.Equal(slog.Attr{}) {
		return buffer
	}
	if value.Kind() == slog.KindGroup {
		before := len(buffer)
		buffer = append(buffer, separator...)
		buffer = append(buffer, '(')
		start := len(buffer)
		for _, child := range value.Group() {
			sep := " / "
			if len(buffer) == start {
				sep = ""
			}
			buffer = appendPatternAttr(buffer, child, deviceAt, sep)
		}
		if len(buffer) == start {
			return buffer[:before]
		}
		return append(buffer, ')')
	}
	buffer = append(buffer, separator...)
	switch value.Kind() {
	case slog.KindString:
		return appendLogText(buffer, value.String())
	case slog.KindInt64:
		return strconv.AppendInt(buffer, value.Int64(), 10)
	case slog.KindUint64:
		return strconv.AppendUint(buffer, value.Uint64(), 10)
	case slog.KindBool:
		return strconv.AppendBool(buffer, value.Bool())
	case slog.KindFloat64:
		return strconv.AppendFloat(buffer, value.Float64(), 'g', -1, 64)
	case slog.KindDuration:
		return append(buffer, value.Duration().String()...)
	case slog.KindTime:
		return value.Time().In(logZone).AppendFormat(buffer, "2006-01-02 15:04:05.000")
	default:
		return fmt.Append(buffer, value.Any())
	}
}

func appendLogText(buffer []byte, text string) []byte {
	if strings.ContainsAny(text, "\r\n\t") {
		return strconv.AppendQuote(buffer, text)
	}
	return append(buffer, text...)
}

package deviceruntime

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	"droidharbor/internal/devices"
	"droidharbor/internal/diagnostics"
)

type cpuCounters [8]uint64

type cpuSample struct {
	previous cpuCounters
	known    bool
}

func parseCPU(data []byte) (cpuCounters, error) {
	var counters cpuCounters
	line, _, _ := bytes.Cut(data, []byte{'\n'})
	fields := bytes.Fields(line)
	if len(fields) < 9 || !bytes.Equal(fields[0], []byte("cpu")) {
		return counters, diagnostics.New("aggregate CPU counters are missing")
	}
	for i := range counters {
		value, err := strconv.ParseUint(string(fields[i+1]), 10, 64)
		if err != nil {
			return counters, diagnostics.New("invalid aggregate CPU counter")
		}
		counters[i] = value
	}
	return counters, nil
}

func (s *cpuSample) update(current cpuCounters) *float64 {
	previous, known := s.previous, s.known
	s.previous, s.known = current, true
	if !known {
		return nil
	}
	var total, idle uint64
	for i, value := range current {
		if value < previous[i] {
			return nil
		}
		delta := value - previous[i]
		total += delta
		if i == 3 || i == 4 {
			idle += delta
		}
	}
	if total == 0 {
		return nil
	}
	percent := float64(total-idle) * 100 / float64(total)
	return &percent
}

func parseMemory(data []byte) (used, total uint64, err error) {
	var available uint64
	foundTotal, foundAvailable := false, false
	for line := range bytes.Lines(data) {
		key, value, ok := bytes.Cut(line, []byte{':'})
		if !ok || (!bytes.Equal(key, []byte("MemTotal")) && !bytes.Equal(key, []byte("MemAvailable"))) {
			continue
		}
		fields := bytes.Fields(value)
		if len(fields) != 2 || !bytes.Equal(fields[1], []byte("kB")) {
			return 0, 0, diagnostics.New("invalid memory counter unit")
		}
		amount, parseErr := strconv.ParseUint(string(fields[0]), 10, 64)
		if parseErr != nil || amount > math.MaxUint64/1024 {
			return 0, 0, diagnostics.New("invalid memory counter")
		}
		if bytes.Equal(key, []byte("MemTotal")) {
			total, foundTotal = amount*1024, true
		} else {
			available, foundAvailable = amount*1024, true
		}
	}
	if !foundTotal || !foundAvailable || total == 0 || available > total {
		return 0, 0, diagnostics.New("required memory counters are unavailable")
	}
	return total - available, total, nil
}

type batterySample struct {
	temperature, percent float64
	powered              bool
	status               string
}

func parseBattery(data []byte) (batterySample, error) {
	var sample batterySample
	var level, scale, temperature, status int64
	var levelFound, scaleFound, temperatureFound, statusFound, powerFound bool
	for line := range bytes.Lines(data) {
		key, raw, ok := bytes.Cut(bytes.TrimSpace(line), []byte{':'})
		if !ok {
			continue
		}
		value := bytes.TrimSpace(raw)
		switch string(key) {
		case "AC powered", "USB powered", "Wireless powered", "Dock powered":
			if !bytes.Equal(value, []byte("true")) && !bytes.Equal(value, []byte("false")) {
				return sample, diagnostics.New("invalid battery power state")
			}
			powerFound = true
			sample.powered = sample.powered || bytes.Equal(value, []byte("true"))
		case "level", "scale", "temperature", "status":
			n, err := strconv.ParseInt(string(value), 10, 32)
			if err != nil {
				return sample, diagnostics.New("invalid battery counter")
			}
			switch string(key) {
			case "level":
				level, levelFound = n, true
			case "scale":
				scale, scaleFound = n, true
			case "temperature":
				temperature, temperatureFound = n, true
			case "status":
				status, statusFound = n, true
			}
		}
	}
	if !levelFound || !scaleFound || !temperatureFound || !statusFound || !powerFound || scale <= 0 || level < 0 || level > scale || temperature < -1000 || temperature > 2000 {
		return sample, diagnostics.New("required battery counters are unavailable")
	}
	switch status {
	case 1:
		sample.status = "unknown"
	case 2:
		sample.status = "charging"
	case 3:
		sample.status = "discharging"
	case 4:
		sample.status = "not-charging"
	case 5:
		sample.status = "full"
	default:
		return sample, diagnostics.New("invalid battery charge status")
	}
	sample.temperature, sample.percent = float64(temperature)/10, float64(level)*100/float64(scale)
	return sample, nil
}

func (w *worker) readMetric(command string, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(w.ctx, 3*time.Second)
	defer cancel()
	result, err := w.manager.adb.RunShell(ctx, w.link.TransportID, command, limit, w.authorize)
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, diagnostics.New(fmt.Sprintf("metric command failed (exit %d)", result.ExitCode))
	}
	return result.Stdout, nil
}

func (w *worker) sampleMetrics() {
	metrics := &devices.Metrics{}
	cpuData, err := w.readMetric("head -n 1 /proc/stat", 1024)
	if err == nil {
		var current cpuCounters
		current, err = parseCPU(cpuData)
		if err == nil {
			metrics.CPUPercent = w.cpu.update(current)
		}
	}
	if err != nil {
		w.cpu.known = false
	}
	w.issue("cpu", "CPU_METRIC_UNAVAILABLE", "无法读取 CPU 使用率", err)
	memoryData, err := w.readMetric("cat /proc/meminfo", 64<<10)
	if err == nil {
		var used, total uint64
		used, total, err = parseMemory(memoryData)
		if err == nil {
			metrics.MemoryUsedBytes, metrics.MemoryTotalBytes = &used, &total
		}
	}
	w.issue("memory", "MEMORY_METRIC_UNAVAILABLE", "无法读取内存使用情况", err)
	batteryData, err := w.readMetric("dumpsys battery", 16<<10)
	if err == nil {
		var battery batterySample
		battery, err = parseBattery(batteryData)
		if err == nil {
			metrics.BatteryTemperatureC, metrics.BatteryPercent = &battery.temperature, &battery.percent
			metrics.PowerConnected, metrics.ChargingStatus = &battery.powered, &battery.status
		}
	}
	w.issue("battery", "BATTERY_METRIC_UNAVAILABLE", "无法读取电池状态", err)
	if w.ctx.Err() != nil || w.authorize() != nil {
		return
	}
	metrics.ObservedAt = time.Now().UTC()
	w.state.Metrics = metrics
	w.publish()
}

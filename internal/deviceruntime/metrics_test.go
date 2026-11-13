package deviceruntime

import (
	"math"
	"testing"
)

func TestCPUUsesDeltasExcludesGuestAndResetsAcrossCounterRollback(t *testing.T) {
	first, err := parseCPU([]byte("cpu 100 0 100 800 0 0 0 0 500 900\ncpu0 1 2 3\n"))
	if err != nil {
		t.Fatal(err)
	}
	var sample cpuSample
	if got := sample.update(first); got != nil {
		t.Fatalf("first sample reported CPU usage: %v", *got)
	}
	second, err := parseCPU([]byte("cpu 120 0 110 870 10 0 0 0 9000 20000\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := sample.update(second)
	if got == nil || math.Abs(*got-100.0*30/110) > 1e-9 {
		t.Fatalf("aggregate CPU usage = %v", got)
	}
	if got := sample.update(cpuCounters{1, 0, 1, 8, 0, 0, 0, 0}); got != nil {
		t.Fatal("counter rollback produced a percentage")
	}
	got = sample.update(cpuCounters{3, 0, 2, 15, 0, 0, 0, 0})
	if got == nil || *got != 30 {
		t.Fatalf("CPU did not recover from its new baseline: %v", got)
	}
}

func TestMemoryUsesAvailableRatherThanFreeAndRequiresAvailable(t *testing.T) {
	used, total, err := parseMemory([]byte("MemTotal: 8192 kB\nMemFree: 1024 kB\nMemAvailable: 2048 kB\n"))
	if err != nil || used != 6144*1024 || total != 8192*1024 {
		t.Fatalf("memory = %d/%d, err=%v", used, total, err)
	}
	if _, _, err := parseMemory([]byte("MemTotal: 8192 kB\nMemFree: 1024 kB\n")); err == nil {
		t.Fatal("missing MemAvailable was replaced with a different counter")
	}
	if _, _, err := parseMemory([]byte("MemTotal: 8192 kB\nMemAvailable: 9000 kB\n")); err == nil {
		t.Fatal("invalid memory counters were published")
	}
}

func TestBatterySeparatesPowerFromChargeAndUsesScaleAndTemperatureUnits(t *testing.T) {
	data := []byte("Current Battery Service state:\n AC powered: true\n USB powered: false\n Wireless powered: false\n status: 4\n level: 100\n scale: 200\n temperature: 230\n")
	got, err := parseBattery(data)
	if err != nil || !got.powered || got.status != "not-charging" || got.percent != 50 || got.temperature != 23 {
		t.Fatalf("battery=%+v err=%v", got, err)
	}
	if _, err := parseBattery([]byte("AC powered: false\nstatus: 2\nlevel: 50\nscale: 0\ntemperature: 230\n")); err == nil {
		t.Fatal("invalid battery scale was reported as a percentage")
	}
	if _, err := parseBattery([]byte("AC powered: true\nstatus: 5\nlevel: 100\nscale: 100\n")); err == nil {
		t.Fatal("missing temperature was shown as zero")
	}
}

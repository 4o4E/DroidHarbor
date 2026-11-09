package capture

import (
	"strings"
	"testing"
)

func TestParseReadyLineRequiresCompleteIdentity(t *testing.T) {
	got, err := parseReadyLine("DHREADY 123 550e8400-e29b-41d4-a716-446655440000 987")
	if err != nil || got.pid != 123 || got.boot != "550e8400-e29b-41d4-a716-446655440000" || got.ticks != 987 {
		t.Fatalf("parseReadyLine() = %+v, %v", got, err)
	}
	for _, line := range []string{"", "DHREADY 0 boot 2", "DHREADY 1 boot 0", "DHREADY 1 boot nope", "noise DHREADY 1 boot 2"} {
		if _, err := parseReadyLine(line); err == nil {
			t.Fatalf("parseReadyLine(%q) succeeded", line)
		}
	}
}

func TestStartupCommandGatesExecOnNonceAndGO(t *testing.T) {
	command, err := startupCommand("/data/local/tmp/droidharbor-abc.jar", "gate-secret", 7, Settings{MaxSize: 1080, MaxFPS: 30, BitRate: 2000000})
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"DHREADY", "read -r gate || exit 1", "= 'gate-secret'", "read -r go || exit 1", "= GO", "exec 'app_process' '/system/bin'", "CLASSPATH='/data/local/tmp/droidharbor-abc.jar'"} {
		if !strings.Contains(command, required) {
			t.Fatalf("startup command lacks %q: %s", required, command)
		}
	}
	if strings.Contains(command, "--nice-name") {
		t.Fatalf("startup command passes unsupported app_process --nice-name: %s", command)
	}
	if strings.Contains(command, "&") {
		t.Fatalf("foreground command backgrounds app_process: %s", command)
	}
}

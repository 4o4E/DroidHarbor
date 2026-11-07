package capture

import (
	"strings"
	"testing"
)

func TestProcessStartTicksHandlesSpacesAndParenthesesInComm(t *testing.T) {
	fields := []string{"S"}
	for i := 1; i < 19; i++ {
		fields = append(fields, "0")
	}
	fields = append(fields, "987654")
	stat := "123 (worker with ) paren) " + strings.Join(fields, " ")
	got, err := processStartTicks(stat)
	if err != nil || got != 987654 {
		t.Fatalf("processStartTicks() = %d, %v; want 987654", got, err)
	}
}

func TestProcessStartTicksRejectsIncompleteAndZero(t *testing.T) {
	for _, stat := range []string{"123 (worker", "123 (worker) S 0", "123 (worker) S 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0"} {
		if _, err := processStartTicks(stat); err == nil {
			t.Fatalf("processStartTicks(%q) succeeded", stat)
		}
	}
}

func TestValidBootUUID(t *testing.T) {
	if !validBootUUID("550e8400-e29b-41d4-a716-446655440000") {
		t.Fatal("valid UUID rejected")
	}
	for _, value := range []string{"", "550e8400-e29b-41d4-a716-44665544000z", "550e8400e29b-41d4-a716-446655440000"} {
		if validBootUUID(value) {
			t.Fatalf("invalid UUID accepted: %q", value)
		}
	}
}

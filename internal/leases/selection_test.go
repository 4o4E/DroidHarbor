package leases

import (
	"context"
	"droidharbor/internal/identity"
	"errors"
	"strings"
	"testing"
)

func TestSelectedDeviceNeverFallsThroughAndReplayKeepsOriginalLease(t *testing.T) {
	m, db, source, _, bearer := newLeaseTestManager(t, 2)
	if _, err := db.Exec(`UPDATE devices SET display_name='9R' WHERE id='b'`); err != nil {
		t.Fatal(err)
	}
	request := ClaimRequest{TaskID: "selected", TaskSecret: identity.Secret(), IdempotencyKey: identity.Secret(), DeviceName: "9R"}
	source.setLinks(Link{DeviceID: "a", LinkID: "link-a", TransportID: 1, Generation: 1})
	if _, err := m.Claim(context.Background(), bearer, request); err == nil {
		t.Fatal("offline selected phone silently replaced with another phone")
	} else {
		var e *Error
		if !errors.As(err, &e) || e.Code != "DEVICE_UNAVAILABLE" {
			t.Fatal(err)
		}
	}
	source.setLinks(Link{DeviceID: "a", LinkID: "link-a", TransportID: 1, Generation: 1}, Link{DeviceID: "b", LinkID: "link-b", TransportID: 2, Generation: 1})
	request.RequiredTags = []string{"shopping"}
	if _, err := m.Claim(context.Background(), bearer, request); err == nil {
		t.Fatal("selector bypassed required tags")
	}
	if _, err := db.Exec(`INSERT INTO device_tags VALUES('b','shopping')`); err != nil {
		t.Fatal(err)
	}
	lease, err := m.Claim(context.Background(), bearer, request)
	if err != nil || lease.DeviceID != "b" {
		t.Fatalf("wrong selected phone: %+v %v", lease, err)
	}
	if _, err := db.Exec(`UPDATE devices SET display_name='9R' WHERE id='a'`); err != nil {
		t.Fatal(err)
	}
	replay, err := m.Claim(context.Background(), bearer, request)
	if err != nil || replay.ID != lease.ID || replay.DeviceID != "b" {
		t.Fatalf("replay re-resolved the name: %+v %v", replay, err)
	}
	changed := request
	changed.DeviceName = ""
	changed.DeviceID = "a"
	if _, err := m.Claim(context.Background(), bearer, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("selector absent from idempotency identity: %v", err)
	}
}

func TestDuplicateNamesRequireCompleteIDBeforeAvailabilityFiltering(t *testing.T) {
	m, db, _, _, bearer := newLeaseTestManager(t, 2)
	if _, err := db.Exec(`UPDATE devices SET display_name='same'; UPDATE devices SET allocation_state='maintenance',maintenance_requested=1 WHERE id='a'`); err != nil {
		t.Fatal(err)
	}
	request := ClaimRequest{TaskID: "duplicate", TaskSecret: identity.Secret(), IdempotencyKey: identity.Secret(), DeviceName: "same"}
	_, err := m.Claim(context.Background(), bearer, request)
	var e *Error
	if !errors.As(err, &e) || e.Code != "DEVICE_NAME_AMBIGUOUS" || !strings.Contains(e.Message, "a、b") {
		t.Fatalf("ambiguous name selected available phone: %v", err)
	}
	request.DeviceName = ""
	request.DeviceID = "b"
	lease, err := m.Claim(context.Background(), bearer, request)
	if err != nil || lease.DeviceID != "b" {
		t.Fatalf("complete ID did not resolve ambiguity: %+v %v", lease, err)
	}
}

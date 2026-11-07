package leases

import (
	"context"
	"testing"

	"droidharbor/internal/events"
	"droidharbor/internal/identity"
)

func TestLeaseEventsTrackOwnerAndTerminalInvalidation(t *testing.T) {
	manager, _, _, _, bearer := newLeaseTestManager(t, 1)
	bus := events.New(manager.BootID())
	defer bus.Close()
	manager.SetEvents(bus)
	subscription, err := bus.Subscribe(events.Scope{Admin: true})
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	<-subscription.C // initial resync

	secret := identity.Secret()
	lease := claimForTest(t, manager, bearer, "event-task", secret, "event-key")
	if got := manager.EventTask(lease.DeviceID); got != lease.TaskID {
		t.Fatalf("EventTask after claim = %q, want %q", got, lease.TaskID)
	}
	for _, entity := range []string{"lease", "device"} {
		select {
		case event := <-subscription.C:
			if event.EntityType != entity || event.TaskID != lease.TaskID || event.DeviceID != lease.DeviceID {
				t.Fatalf("claim event = %+v, want %s event routed to task and device", event, entity)
			}
		case <-subscription.Done:
			t.Fatal("subscription closed while reading claim events")
		default:
			t.Fatal("claim did not publish its state change")
		}
	}
	subscription.Close()
	subscription, err = bus.Subscribe(events.Scope{TaskID: lease.TaskID})
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	<-subscription.C

	principal, err := manager.Authenticate(context.Background(), secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = manager.Release(context.Background(), principal, lease.ID); err != nil {
		t.Fatal(err)
	}
	if got := manager.EventTask(lease.DeviceID); got != "" {
		t.Fatalf("EventTask after terminal transition = %q, want empty", got)
	}
	for _, entity := range []string{"lease", "device"} {
		select {
		case event := <-subscription.C:
			if event.EntityType != entity || event.TaskID != lease.TaskID || event.DeviceID != lease.DeviceID {
				t.Fatalf("terminal event = %+v, want %s event scoped to terminal task", event, entity)
			}
		case <-subscription.Done:
			t.Fatal("subscription closed while reading terminal events")
		default:
			t.Fatal("terminal transition did not notify its task")
		}
	}
}

func TestEventTaskCompareAndDeletePreservesNewOwner(t *testing.T) {
	manager := &Manager{}
	manager.setEventOwner("device", "new-task")
	manager.clearEventOwner("device", "old-task")
	if got := manager.EventTask("device"); got != "new-task" {
		t.Fatalf("stale terminal owner removal changed EventTask to %q", got)
	}
}
func TestLeaseTerminalEventSurvivesPersistenceFailure(t *testing.T) {
	manager, db, _, _, bearer := newLeaseTestManager(t, 1)
	bus := events.New(manager.BootID())
	defer bus.Close()
	manager.SetEvents(bus)
	secret := identity.Secret()
	lease := claimForTest(t, manager, bearer, "failure-task", secret, "failure-key")
	subscription, err := bus.Subscribe(events.Scope{TaskID: lease.TaskID})
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	<-subscription.C

	if _, err = db.Exec(`CREATE TRIGGER fail_lease_end BEFORE UPDATE OF status ON leases WHEN NEW.status='ended' BEGIN SELECT RAISE(ABORT, 'forced failure'); END`); err != nil {
		t.Fatal(err)
	}
	principal, err := manager.Authenticate(context.Background(), secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = manager.Release(context.Background(), principal, lease.ID); err == nil {
		t.Fatal("Release succeeded despite the persistence trigger")
	}
	if got := manager.EventTask(lease.DeviceID); got != "" {
		t.Fatalf("EventTask after failed terminal persistence = %q, want empty", got)
	}
	for _, entity := range []string{"lease", "device"} {
		select {
		case event := <-subscription.C:
			if event.EntityType != entity || event.TaskID != lease.TaskID || event.DeviceID != lease.DeviceID {
				t.Fatalf("failed-persistence terminal event = %+v, want %s event", event, entity)
			}
		case <-subscription.Done:
			t.Fatal("subscription closed while reading terminal events")
		default:
			t.Fatal("runtime revocation did not notify its task")
		}
	}
}

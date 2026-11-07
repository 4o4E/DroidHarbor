package events

import (
	"testing"
)

func TestSubscribeResyncAndTaskScope(t *testing.T) {
	bus := New("boot-a")
	defer bus.Close()
	sub, err := bus.Subscribe(Scope{TaskID: "task-a"})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	resync := <-sub.C
	if resync.EntityType != "resync" || resync.BootID != "boot-a" || resync.EntityVersion != 0 {
		t.Fatalf("bad resync: %+v", resync)
	}
	if _, err = bus.Publish(Change{EntityType: "device", EntityID: "device-a", DeviceID: "device-a", TaskID: "task-b"}); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-sub.C:
		t.Fatalf("foreign event delivered: %+v", e)
	default:
	}
	e, err := bus.Publish(Change{EntityType: "lease", EntityID: "lease-a", TaskID: "task-a", DeviceID: "device-a"})
	if err != nil {
		t.Fatal(err)
	}
	got := <-sub.C
	if got.EventID != "Ym9vdC1h:2" || got.EntityVersion != e.EntityVersion || got.EntityType != "lease" {
		t.Fatalf("bad task event: %+v", got)
	}
}

func TestSlowSubscriberRemovedAndClosed(t *testing.T) {
	bus := New("boot-b")
	defer bus.Close()
	sub, err := bus.Subscribe(Scope{Admin: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < QueueCapacity; i++ {
		if _, err = bus.Publish(Change{EntityType: "caller", EntityID: "caller"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = bus.Publish(Change{EntityType: "caller", EntityID: "caller"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sub.Done:
	default:
		t.Fatal("overflow did not signal subscriber closure")
	}
	for range QueueCapacity + 1 {
		if _, open := <-sub.C; !open {
			break
		}
	}
	if _, open := <-sub.C; open {
		t.Fatal("overflowed subscriber channel remained open")
	}
	if len(bus.subs) != 0 {
		t.Fatal("overflowed subscriber retained")
	}
}

func TestSubscriptionLimitAndClose(t *testing.T) {
	bus := New("boot-c")
	for i := 0; i < MaxSubscriptions; i++ {
		if _, err := bus.Subscribe(Scope{Admin: true}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := bus.Subscribe(Scope{Admin: true}); err != ErrCapacity {
		t.Fatalf("capacity error = %v", err)
	}
	bus.Close()
	if _, err := bus.Subscribe(Scope{Admin: true}); err != ErrClosed {
		t.Fatalf("closed bus error = %v", err)
	}
}

package events

import "testing"

func TestDeviceRuntimeChangesReachAdministratorsButNotTasks(t *testing.T) {
	bus := New("runtime-boot")
	defer bus.Close()
	admin, err := bus.Subscribe(Scope{Admin: true})
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	task, err := bus.Subscribe(Scope{TaskID: "task"})
	if err != nil {
		t.Fatal(err)
	}
	defer task.Close()
	<-admin.C
	<-task.C
	published, err := bus.Publish(Change{EntityType: "device-runtime", EntityID: "phone", DeviceID: "phone", TaskID: "task"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-admin.C:
		if got.EventID != published.EventID || got.EntityID != "phone" || got.EntityType != "device-runtime" {
			t.Fatalf("wrong administrator invalidation: %+v", got)
		}
	default:
		t.Fatal("administrator did not receive runtime invalidation")
	}
	select {
	case got := <-task.C:
		t.Fatalf("private runtime event leaked to task: %+v", got)
	default:
	}
}

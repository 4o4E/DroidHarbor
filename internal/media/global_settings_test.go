package media

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"droidharbor/internal/capture"
	"droidharbor/internal/events"
	"droidharbor/internal/identity"
	"droidharbor/internal/storage"
)

func TestGlobalTargetIsSharedPersistentAndDoesNotChangeExistingCapture(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "video.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	now := time.Now()
	if _, err = db.Exec(`INSERT INTO admin_sessions(id,secret_hash,admin_credential_version,created_at,expires_at) VALUES('owner',?,'v',?,?)`, identity.Digest("test"), now.UnixMilli(), now.Add(time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO devices(id,display_name,allocation_state) VALUES('a','phone-a','available'),('b','phone-b','available')`); err != nil {
		t.Fatal(err)
	}
	m := &Manager{ctx: ctx, deps: Dependencies{DB: db, AdminVersion: "v", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, hubs: make(map[string]*hub)}
	source := &capture.Stream{}
	old := VideoParameters{MaxSize: 1920, MaxFPS: 60, BitRate: 12000000}
	m.hubs["a"] = &hub{manager: m, deviceID: "a", source: source, requested: old, revision: 1, globalRevision: 1, captureRevision: 1, epoch: 9, effective: &EffectiveVideo{Parameters: old, Revision: 1, GlobalRevision: 1}, status: "running"}
	fps, rate := 90, 20000000
	saved, err := m.PatchGlobalVideo(ctx, "owner", GlobalVideoPatch{Revision: 1, MaxFPS: &fps, BitRate: &rate})
	if err != nil || saved.Revision != 2 || saved.Requested != (VideoQuality{MaxFPS: fps, BitRate: rate}) {
		t.Fatalf("save failed: %+v %v", saved, err)
	}
	active := m.hubs["a"]
	if active.source != source || active.requested != old || active.epoch != 9 || active.status != "running" {
		t.Fatal("saving global target changed a live capture")
	}
	current, err := m.Video(ctx, "owner", "a")
	if err != nil || !current.Pending || current.Effective.Parameters != old || current.Requested.MaxFPS != fps {
		t.Fatalf("pending/actual contract lost: %+v %v", current, err)
	}
	size := old.MaxSize
	sizeSnapshot, err := m.PatchVideo(ctx, "owner", "a", VideoPatch{Revision: 1, MaxSize: &size})
	if err != nil || sizeSnapshot.GlobalRevision != 2 || sizeSnapshot.Requested.MaxFPS != fps || !sizeSnapshot.Pending {
		t.Fatalf("device size response lost the global target and actual pending state: %+v %v", sizeSnapshot, err)
	}
	for _, id := range []string{"a", "b"} {
		target, _, revision, profile, err := m.captureConfiguration(ctx, id)
		if err != nil || target.MaxFPS != fps || target.BitRate != rate || revision != 2 || profile != nil {
			t.Fatalf("fresh capture target differs for %s: %+v %d %+v %v", id, target, revision, profile, err)
		}
	}
	_, err = m.PatchGlobalVideo(ctx, "owner", GlobalVideoPatch{Revision: 1, MaxFPS: &fps})
	var conflict *Error
	if !errors.As(err, &conflict) || conflict.Code != "VIDEO_SETTINGS_CONFLICT" {
		t.Fatalf("stale edit overwrote global target: %v", err)
	}
	persisted, err := m.GlobalVideo(ctx, "owner")
	if err != nil || persisted.Revision != 2 || persisted.Requested != saved.Requested {
		t.Fatalf("target not persisted: %+v %v", persisted, err)
	}
	t.Run("revoked startup does not preserve stale fallback after a new target", func(t *testing.T) {
		m.deps.Events = events.New("video-test")
		defer m.deps.Events.Close()
		m.deps.TaskOwner = func(string) string { return "" }
		active.source, active.starting = nil, true
		active.fallback = &VideoFallback{DeviceID: "a", Revision: 2, Status: "failed"}
		if err := active.startSource(ctx, func() error { return context.Canceled }); !errors.Is(err, context.Canceled) {
			t.Fatalf("revoked startup did not fail: %v", err)
		}
		nextFPS := 120
		next, err := m.PatchGlobalVideo(ctx, "owner", GlobalVideoPatch{Revision: 2, MaxFPS: &nextFPS})
		if err != nil || len(next.Fallbacks) != 0 {
			t.Fatalf("new global target retained a canceled inactive startup: %+v %v", next, err)
		}
	})
}

func TestFallbackUsesSuccessfulProfileWithinTargetAndRetriesNewRevision(t *testing.T) {
	target := VideoParameters{MaxSize: 1920, MaxFPS: 90, BitRate: 20000000}
	good := &videoProfile{TargetRevision: 1, Target: target.quality(), Applied: VideoParameters{MaxSize: 1920, MaxFPS: 60, BitRate: 12000000}}
	choices := captureChoices(target, 2, good)
	if choices.count != 3 || choices.values[0] != target || choices.values[1] != good.Applied || choices.values[2] != (VideoParameters{MaxSize: 1920, MaxFPS: 30, BitRate: 4000000}) {
		t.Fatalf("new target order invalid: %+v", choices)
	}
	good.TargetRevision = 2
	choices = captureChoices(target, 2, good)
	if choices.count != 2 || choices.values[0] != good.Applied {
		t.Fatalf("known rejection retried on same target: %+v", choices)
	}
	lower := VideoParameters{MaxSize: 1920, MaxFPS: 30, BitRate: 2000000}
	choices = captureChoices(lower, 3, good)
	if choices.count != 1 || choices.values[0] != lower {
		t.Fatalf("fallback exceeded global target: %+v", choices)
	}
	good.Applied.MaxSize = 960
	choices = captureChoices(target, 3, good)
	if choices.count != 2 || choices.values[0] != target || choices.values[1].MaxSize != 1920 {
		t.Fatalf("fallback silently changed resolution: %+v", choices)
	}
}

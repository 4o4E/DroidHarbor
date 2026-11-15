package storage

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

func TestAudioGainMigrationPreservesPreferencesAndDevices(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema-four.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	execute(t, db, "CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY,applied_at INTEGER NOT NULL) STRICT")
	for version := 1; version <= 4; version++ {
		migration, err := migrations.ReadFile(fmt.Sprintf("migrations/%03d_initial.sql", version))
		if err != nil {
			t.Fatal(err)
		}
		execute(t, db, string(migration))
		execute(t, db, "INSERT INTO schema_migrations VALUES(?,1)", version)
	}
	execute(t, db, "INSERT INTO devices(id,display_name,allocation_state) VALUES('phone','保留设备','maintenance')")
	execute(t, db, "UPDATE system_preferences SET audio_default='device',window_scales='[0.75,2]',revision=7")
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var audio, scales, name string
	var revision int
	var maxGain float64
	if err = db.QueryRow("SELECT audio_default,window_scales,revision,audio_gain_max FROM system_preferences WHERE id=1").Scan(&audio, &scales, &revision, &maxGain); err != nil {
		t.Fatal(err)
	}
	if audio != "device" || scales != "[0.75,2]" || revision != 7 || maxGain != 3 {
		t.Fatalf("preferences changed: %q %q %d %v", audio, scales, revision, maxGain)
	}
	if err = db.QueryRow("SELECT display_name FROM devices WHERE id='phone'").Scan(&name); err != nil || name != "保留设备" {
		t.Fatalf("device changed: %q %v", name, err)
	}
}

package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func execute(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func TestAllocationConstraints(t *testing.T) {
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "pool.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	execute(t, db, "INSERT INTO caller_credentials VALUES('c1','one',zeroblob(32),1,NULL),('c2','two',randomblob(32),1,NULL)")
	execute(t, db, "INSERT INTO devices(id,display_name,allocation_state) VALUES('d1','one','available'),('d2','two','available')")
	execute(t, db, "INSERT INTO tasks VALUES('t1','c1','task-one',randomblob(32),1),('t2','c1','task-two',randomblob(32),1),('t3','c2','task-three',randomblob(32),1)")
	insert := "INSERT INTO leases(id,task_id,caller_id,device_id,generation,boot_id,status,claimed_at,expires_at,claim_idempotency_key,claim_request_hash) VALUES(?,?,?,?,?,'boot','active',1,900001,?,zeroblob(32))"
	execute(t, db, insert, "l1", "t1", "c1", "d1", 1, "key1")
	for _, tc := range []struct {
		name, id, task, caller, device string
		generation                     int
		key                            string
	}{
		{"device already active", "l2", "t2", "c1", "d1", 2, "key2"},
		{"task already active", "l2", "t1", "c1", "d2", 1, "key2"},
		{"caller mismatch", "l2", "t3", "c1", "d2", 1, "key2"},
		{"caller idempotency collision", "l2", "t2", "c1", "d2", 1, "key1"},
		{"unknown device", "l2", "t2", "c1", "missing", 1, "key2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.Exec(insert, tc.id, tc.task, tc.caller, tc.device, tc.generation, tc.key); err == nil {
				t.Fatal("非法认领未被约束拒绝")
			}
		})
	}
	execute(t, db, "UPDATE leases SET status='ended',ended_at=2,end_reason='released' WHERE id='l1'")
	execute(t, db, insert, "l2", "t2", "c1", "d1", 2, "key2")
	execute(t, db, insert, "l3", "t3", "c2", "d2", 1, "key1")
	var active int
	if err = db.QueryRow("SELECT count(*) FROM leases WHERE status='active'").Scan(&active); err != nil || active != 2 {
		t.Fatalf("active=%d err=%v", active, err)
	}
}

func TestUnknownDatabasePreserved(t *testing.T) {
	for _, version := range []string{"untracked", "future"} {
		t.Run(version, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "existing.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			execute(t, db, "CREATE TABLE original(value TEXT); INSERT INTO original VALUES('keep')")
			if version == "future" {
				execute(t, db, "CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY,applied_at INTEGER); INSERT INTO schema_migrations VALUES(99,1)")
			}
			if err = db.Close(); err != nil {
				t.Fatal(err)
			}
			if reopened, err := Open(context.Background(), path); err == nil {
				reopened.Close()
				t.Fatal("未知数据库被接受")
			}
			db, err = sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var value string
			if err = db.QueryRow("SELECT value FROM original").Scan(&value); err != nil || value != "keep" {
				t.Fatalf("existing data changed: %q %v", value, err)
			}
		})
	}
}

func TestFailedMigrationRollsBackAndCanResume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	execute(t, db, "CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY,applied_at INTEGER NOT NULL) STRICT; CREATE TABLE devices(blocker TEXT)")
	if err = migrate(context.Background(), db); err == nil {
		t.Fatal("迁移冲突未失败")
	}
	var created, version int
	if err = db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE name='caller_credentials'").Scan(&created); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow("SELECT count(*) FROM schema_migrations").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if created != 0 || version != 0 {
		t.Fatalf("部分迁移泄漏：tables=%d versions=%d", created, version)
	}
	execute(t, db, "DROP TABLE devices")
	if err = migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err = migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow("SELECT max(version) FROM schema_migrations").Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
}

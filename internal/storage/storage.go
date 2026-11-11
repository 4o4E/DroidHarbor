// Package storage 管理本地元数据；网络与手机操作不得放进数据库事务。
package storage

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

const SchemaVersion = 2

func Open(ctx context.Context, path string) (*sql.DB, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(absolute), 0700); err != nil {
		return nil, err
	}
	// 文件只读写元数据；拒绝未知已有库，不能用 CREATE IF NOT EXISTS 掩盖错误版本。
	uriPath := filepath.ToSlash(absolute)
	if filepath.VolumeName(absolute) != "" {
		uriPath = "/" + uriPath
	}
	uri := url.URL{Scheme: "file", Path: uriPath}
	query := uri.Query()
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "journal_mode(WAL)")
	query.Add("_pragma", "synchronous(FULL)")
	query.Set("_txlock", "immediate")
	uri.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err = migrate(ctx, db); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return db, nil
}

func migrate(ctx context.Context, db *sql.DB) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		rollback := tx.Rollback()
		if rollback != nil && !errors.Is(rollback, sql.ErrTxDone) {
			err = errors.Join(err, rollback)
		}
	}()
	var tables, tracking int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%'").Scan(&tables); err != nil {
		return err
	}
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE type='table' AND name='schema_migrations'").Scan(&tracking); err != nil {
		return err
	}
	if tracking == 0 {
		if tables != 0 {
			return fmt.Errorf("数据库缺少迁移记录，拒绝覆盖已有 %d 张表", tables)
		}
		if _, err = tx.ExecContext(ctx, "CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL) STRICT"); err != nil {
			return err
		}
	}
	var count, version int
	if err = tx.QueryRowContext(ctx, "SELECT count(*), coalesce(max(version),0) FROM schema_migrations").Scan(&count, &version); err != nil {
		return err
	}
	if version > SchemaVersion || count != version {
		return fmt.Errorf("不支持的数据库迁移历史：version=%d count=%d", version, count)
	}
	for next := version + 1; next <= SchemaVersion; next++ {
		migration, readErr := migrations.ReadFile(fmt.Sprintf("migrations/%03d_initial.sql", next))
		if readErr != nil {
			return readErr
		}
		if _, err = tx.ExecContext(ctx, string(migration)); err != nil {
			return fmt.Errorf("迁移 %d 失败：%w", next, err)
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO schema_migrations VALUES (?,?)", next, time.Now().UTC().UnixMilli()); err != nil {
			return err
		}
	}
	var violations int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM pragma_foreign_key_check").Scan(&violations); err != nil {
		return err
	}
	if violations != 0 {
		return fmt.Errorf("数据库存在 %d 处外键错误", violations)
	}
	return tx.Commit()
}

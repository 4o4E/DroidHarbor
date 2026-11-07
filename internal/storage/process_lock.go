package storage

import (
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
)

// AcquireProcessLock 在迁移或启动恢复之前占有数据库对应的进程锁。
// 锁文件长期保留；删除锁文件会让等待者与新进程锁住不同 inode。
func AcquireProcessLock(databasePath string) (io.Closer, error) {
	absolute, err := filepath.Abs(databasePath)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(absolute), 0700); err != nil {
		return nil, err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if errors.Is(err, os.ErrNotExist) {
		parent, parentErr := filepath.EvalSymlinks(filepath.Dir(absolute))
		if parentErr != nil {
			return nil, parentErr
		}
		canonical = filepath.Join(parent, filepath.Base(absolute))
	} else if err != nil {
		return nil, err
	}
	lock := flock.New(canonical + ".process.lock")
	locked, err := lock.TryLock()
	if err != nil {
		return nil, errors.Join(err, lock.Close())
	}
	if !locked {
		return nil, errors.Join(errors.New("数据库已由另一个平台进程占用；不允许同时服务或迁移同一数据库"), lock.Close())
	}
	return lock, nil
}

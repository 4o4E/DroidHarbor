package platform

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"droidharbor/internal/identity"
)

const cacheFormatVersion = 1
const maxAppCacheBytes = 16 << 20
const cacheFormatMarker = ".droidharbor-cache-format"

// Cache contains only rebuildable platform data, never authorization state.
type Cache struct {
	root    *os.Root
	version string
	mu      sync.Mutex
}

func OpenCache(directory string) (*Cache, error) {
	return openCache(directory, cacheFormatVersion)
}

func openCache(directory string, version int) (*Cache, error) {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	cache := &Cache{root: root, version: "v" + strconv.Itoa(version)}
	if err := cache.prepareFormat(); err != nil {
		return nil, errors.Join(err, cache.Close())
	}
	return cache, nil
}

func (c *Cache) prepareFormat() error {
	marker := filepath.Join(c.version, cacheFormatMarker)
	if err := c.root.Mkdir(c.version, 0700); err == nil {
		if err := c.root.WriteFile(marker, []byte(c.version+"\n"), 0600); err != nil {
			return errors.Join(err, c.root.Remove(c.version))
		}
	} else if !errors.Is(err, os.ErrExist) {
		return err
	}
	current, err := c.root.ReadFile(marker)
	if err != nil {
		return fmt.Errorf("read cache format marker: %w", err)
	}
	if string(current) != c.version+"\n" {
		return errors.New("cache format directory is not owned by DroidHarbor")
	}
	directory, err := c.root.Open(".")
	if err != nil {
		return err
	}
	entries, readErr := directory.ReadDir(-1)
	if err := errors.Join(readErr, directory.Close()); err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		suffix, versioned := strings.CutPrefix(name, "v")
		n, err := strconv.Atoi(suffix)
		if !versioned || err != nil || n <= 0 || strconv.Itoa(n) != suffix || name == c.version || !entry.IsDir() {
			continue
		}
		oldMarker, err := c.root.ReadFile(filepath.Join(name, cacheFormatMarker))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if string(oldMarker) != name+"\n" {
			continue
		}
		if err := c.root.RemoveAll(name); err != nil {
			return err
		}
	}
	return nil
}

func (c *Cache) Close() error { return c.root.Close() }

func (c *Cache) appFile(deviceID string, user int) (string, error) {
	if !filepath.IsLocal(deviceID) || filepath.Base(deviceID) != deviceID || deviceID == "." || user < 0 {
		return "", errors.New("invalid app cache owner")
	}
	return filepath.Join(c.version, "devices", deviceID, "apps", strconv.Itoa(user), "presentations.json"), nil
}

type cachedAppPresentation struct {
	Version uint64 `json:"versionCode"`
	Locale  string `json:"locale"`
	Name    string `json:"name"`
	Icon    string `json:"icon"`
}

func (c *Cache) readApps(file string) (map[string]cachedAppPresentation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loadApps(file)
}

func (c *Cache) loadApps(file string) (map[string]cachedAppPresentation, error) {
	f, err := c.root.Open(file)
	if errors.Is(err, os.ErrNotExist) {
		return make(map[string]cachedAppPresentation), nil
	}
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(f, maxAppCacheBytes+1))
	if err := errors.Join(readErr, f.Close()); err != nil {
		return nil, err
	}
	var entries map[string]cachedAppPresentation
	if len(data) > maxAppCacheBytes || json.Unmarshal(data, &entries) != nil || entries == nil {
		return nil, errors.New("invalid application presentation cache")
	}
	return entries, nil
}

func (c *Cache) mergeApps(file string, additions map[string]cachedAppPresentation) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	entries, err := c.loadApps(file)
	if err != nil {
		return err
	}
	for pkg, value := range additions {
		entries[pkg] = value
	}
	if err := c.root.MkdirAll(filepath.Dir(file), 0700); err != nil {
		return err
	}
	temp := file + ".tmp-" + identity.Secret()
	f, err := c.root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	writeErr := json.NewEncoder(f).Encode(entries)
	if writeErr == nil {
		if info, err := f.Stat(); err != nil {
			writeErr = err
		} else if info.Size() > maxAppCacheBytes {
			writeErr = fmt.Errorf("application presentation cache exceeds %d bytes", maxAppCacheBytes)
		}
	}
	if err := errors.Join(writeErr, f.Close()); err != nil {
		return errors.Join(err, c.root.Remove(temp))
	}
	if err := c.root.Rename(temp, file); err != nil {
		return errors.Join(err, c.root.Remove(temp))
	}
	return nil
}

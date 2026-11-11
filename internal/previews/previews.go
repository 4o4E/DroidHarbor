// Package previews captures and caches bounded, low-resolution device screenshots.
package previews

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/jpeg"
	_ "image/png"
	"io"
	"sync"
	"time"

	"droidharbor/internal/adb"
	"droidharbor/internal/leases"
	"golang.org/x/image/draw"
)

const (
	maxInputBytes = 32 << 20
	maxPixels     = 16_000_000
	maxEntries    = 128
	maxCacheBytes = 16 << 20
	maxEdge       = 480
	jpegQuality   = 75
	minInterval   = 5 * time.Second
	idleLifetime  = 60 * time.Second
)

// Source provides a selected live link and checks whether that exact
// incarnation remains current. Implementations must not retarget link work.
type Source interface {
	ReadLink(context.Context, string) (leases.Link, context.Context, error)
	HasLink(leases.Link) bool
}

type Snapshot struct {
	JPEG           []byte
	CapturedAt     time.Time
	DeviceID       string
	LinkGeneration uint64
	link           leases.Link
}

type cacheKey struct {
	link leases.Link
}

type result struct {
	snapshot Snapshot
	err      error
}

type cacheEntry struct {
	result
	lastUsed  time.Time
	startedAt time.Time
}

type flight struct {
	done chan struct{}
	result
}

// Manager coalesces captures per device incarnation and keeps only encoded
// JPEGs. Its lifetime context cancels and joins every accepted worker.
type Manager struct {
	ctx    context.Context
	cancel context.CancelFunc
	source Source
	client *adb.Client

	mu        sync.Mutex
	closed    bool
	entries   map[cacheKey]cacheEntry
	lastStart map[cacheKey]time.Time
	flights   map[cacheKey]*flight
	cacheSize int
	wg        sync.WaitGroup
}

func New(ctx context.Context, source Source, client *adb.Client) *Manager {
	lifetime, cancel := context.WithCancel(ctx)
	m := &Manager{ctx: lifetime, cancel: cancel, source: source, client: client,
		entries: make(map[cacheKey]cacheEntry), lastStart: make(map[cacheKey]time.Time),
		flights: make(map[cacheKey]*flight)}
	m.wg.Add(1)
	go m.evictLoop()
	return m
}

func (m *Manager) Get(ctx context.Context, deviceID string) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	link, linkCtx, err := m.source.ReadLink(ctx, deviceID)
	if err != nil {
		return Snapshot{}, err
	}
	key := cacheKey{link: link}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Snapshot{}, errors.New("preview manager closed")
	}
	now := time.Now()
	if cached, ok := m.entries[key]; ok {
		if now.Sub(cached.startedAt) < minInterval {
			cached.lastUsed = now
			m.entries[key] = cached
			m.mu.Unlock()
			return m.validate(ctx, link, cached.snapshot, nil)
		}
		m.cacheSize -= len(cached.snapshot.JPEG)
		delete(m.entries, key)
	}
	if pending := m.flights[key]; pending != nil {
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return Snapshot{}, ctx.Err()
		case <-pending.done:
			return m.validate(ctx, link, pending.snapshot, pending.err)
		}
	}
	if started := m.lastStart[key]; !started.IsZero() && now.Sub(started) < minInterval {
		m.mu.Unlock()
		return Snapshot{}, ErrRateLimited
	}
	if _, known := m.lastStart[key]; !known && len(m.lastStart) >= maxEntries {
		m.evictIdleLocked(now)
		if len(m.lastStart) >= maxEntries {
			m.mu.Unlock()
			return Snapshot{}, ErrCapacity
		}
	}
	pending := &flight{done: make(chan struct{})}
	m.flights[key] = pending
	m.lastStart[key] = now
	m.wg.Add(1)
	m.mu.Unlock()
	go m.capture(key, link, linkCtx, pending)

	select {
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	case <-pending.done:
		return m.validate(ctx, link, pending.snapshot, pending.err)
	}
}

// Validate confirms that a response still belongs to the exact selected
// link generation immediately before the caller emits its bytes.
func (m *Manager) Validate(ctx context.Context, snapshot Snapshot) bool {
	return m.isSelected(ctx, snapshot.link) && snapshot.link.DeviceID == snapshot.DeviceID &&
		snapshot.link.Generation == snapshot.LinkGeneration
}

func (m *Manager) isSelected(ctx context.Context, expected leases.Link) bool {
	current, _, err := m.source.ReadLink(ctx, expected.DeviceID)
	return err == nil && current == expected && m.source.HasLink(expected)
}

func (m *Manager) validate(ctx context.Context, link leases.Link, snapshot Snapshot, prior error) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if !m.isSelected(ctx, link) {
		return Snapshot{}, ErrLinkChanged
	}
	if prior != nil {
		return Snapshot{}, prior
	}
	return snapshot, nil
}

func (m *Manager) capture(key cacheKey, link leases.Link, linkCtx context.Context, pending *flight) {
	defer m.wg.Done()
	workCtx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
	stop := context.AfterFunc(linkCtx, cancel)
	defer func() { stop(); cancel() }()

	startedAt := time.Now()
	m.mu.Lock()
	m.lastStart[key] = startedAt
	m.mu.Unlock()
	data, err := m.acquire(workCtx, link)
	capturedAt := time.Now().UTC()
	if err == nil {
		pending.snapshot, err = encode(data, capturedAt, key.link.DeviceID, link.Generation)
		pending.snapshot.link = link
	}
	if err == nil && !m.isSelected(workCtx, link) {
		err = ErrLinkChanged
	}
	m.mu.Lock()
	pending.err = err
	if err == nil && !m.closed {
		if len(pending.snapshot.JPEG) > maxCacheBytes {
			err = ErrCapacity
			pending.err = err
		} else if m.makeRoomLocked(len(pending.snapshot.JPEG)) {
			m.entries[key] = cacheEntry{result: result{snapshot: pending.snapshot}, lastUsed: time.Now(), startedAt: startedAt}
			m.cacheSize += len(pending.snapshot.JPEG)
		} else {
			err = ErrCapacity
			pending.err = err
		}
	}
	delete(m.flights, key)
	close(pending.done)
	m.mu.Unlock()
}

func (m *Manager) acquire(ctx context.Context, link leases.Link) ([]byte, error) {
	authorize := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !m.isSelected(ctx, link) {
			return ErrLinkChanged
		}
		return nil
	}
	stream, err := m.client.Open(ctx, link.TransportID, "exec:screencap -p", authorize)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	data, err := io.ReadAll(io.LimitReader(stream, maxInputBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxInputBytes {
		return nil, ErrInputTooLarge
	}
	if err = authorize(); err != nil {
		return nil, err
	}
	return data, nil
}

func encode(pngBytes []byte, capturedAt time.Time, deviceID string, generation uint64) (Snapshot, error) {
	config, format, err := image.DecodeConfig(bytes.NewReader(pngBytes))
	if err != nil || format != "png" {
		return Snapshot{}, ErrInvalidImage
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width > maxPixels/config.Height {
		return Snapshot{}, ErrImageTooLarge
	}
	img, _, err := image.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return Snapshot{}, ErrInvalidImage
	}
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= 0 || height <= 0 || width > maxPixels/height {
		return Snapshot{}, ErrImageTooLarge
	}
	var output image.Image = img
	if width > maxEdge || height > maxEdge {
		scale := float64(maxEdge) / float64(max(width, height))
		width, height = max(1, int(float64(width)*scale)), max(1, int(float64(height)*scale))
		scaled := image.NewRGBA(image.Rect(0, 0, width, height))
		draw.CatmullRom.Scale(scaled, scaled.Bounds(), img, bounds, draw.Over, nil)
		output = scaled
	}
	var encoded bytes.Buffer
	if err = jpeg.Encode(&encoded, output, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return Snapshot{}, err
	}
	return Snapshot{JPEG: encoded.Bytes(), CapturedAt: capturedAt, DeviceID: deviceID, LinkGeneration: generation}, nil
}

func (m *Manager) makeRoomLocked(size int) bool {
	for len(m.entries) >= maxEntries || m.cacheSize+size > maxCacheBytes {
		var oldestKey cacheKey
		var oldest time.Time
		found := false
		for key, entry := range m.entries {
			if !found || entry.lastUsed.Before(oldest) {
				oldestKey, oldest, found = key, entry.lastUsed, true
			}
		}
		if !found {
			return false
		}
		entry := m.entries[oldestKey]
		m.cacheSize -= len(entry.snapshot.JPEG)
		delete(m.entries, oldestKey)
	}
	return true
}

func (m *Manager) evictLoop() {
	defer m.wg.Done()
	ticker := time.NewTicker(idleLifetime / 2)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
		}
		now := time.Now()
		m.mu.Lock()
		m.evictIdleLocked(now)
		m.mu.Unlock()
	}
}

func (m *Manager) evictIdleLocked(now time.Time) {
	for key, entry := range m.entries {
		if now.Sub(entry.lastUsed) >= idleLifetime {
			m.cacheSize -= len(entry.snapshot.JPEG)
			delete(m.entries, key)
		}
	}
	for key, started := range m.lastStart {
		// Keep rate/capacity ownership while a cached frame or worker remains.
		if _, cached := m.entries[key]; !cached && m.flights[key] == nil && now.Sub(started) >= idleLifetime {
			delete(m.lastStart, key)
		}
	}
}

func (m *Manager) Close() error {
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		m.cancel()
	}
	m.mu.Unlock()
	m.wg.Wait()
	m.mu.Lock()
	clear(m.entries)
	clear(m.lastStart)
	m.cacheSize = 0
	m.mu.Unlock()
	return nil
}

var (
	ErrRateLimited   = errors.New("preview capture rate limited")
	ErrCapacity      = errors.New("preview capacity exhausted")
	ErrLinkChanged   = errors.New("device link changed during preview")
	ErrInputTooLarge = errors.New("screenshot exceeds encoded input limit")
	ErrImageTooLarge = errors.New("screenshot exceeds decoded pixel limit")
	ErrInvalidImage  = errors.New("invalid PNG screenshot")
)

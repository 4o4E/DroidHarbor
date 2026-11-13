package devices

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"droidharbor/internal/adb"
)

func TestDisplayDumpWaitsForSamePhoneAndDrainsBeforeNextRead(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	var workers sync.WaitGroup
	var mu sync.Mutex
	phoneBusy := false
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer conn.Close()
				if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
					t.Error(err)
					return
				}
				readService := func() (string, error) {
					var prefix [4]byte
					if _, err := io.ReadFull(conn, prefix[:]); err != nil {
						return "", err
					}
					n, err := strconv.ParseUint(string(prefix[:]), 16, 16)
					if err != nil {
						return "", err
					}
					body := make([]byte, n)
					_, err = io.ReadFull(conn, body)
					return string(body), err
				}
				service, err := readService()
				var transport int
				if err != nil {
					t.Error(err)
					return
				}
				if _, err = fmt.Sscanf(service, "host:transport-id:%d", &transport); err != nil {
					t.Error(err)
					return
				}
				if _, err = io.WriteString(conn, "OKAY"); err != nil {
					t.Error(err)
					return
				}
				service, err = readService()
				if err != nil || service != "exec:dumpsys display" {
					t.Errorf("display service=%q err=%v", service, err)
					return
				}
				if transport != 3 {
					mu.Lock()
					if phoneBusy {
						mu.Unlock()
						if _, err = io.WriteString(conn, "OKAYOne dump is in service already.\n"); err != nil {
							t.Error(err)
						}
						return
					}
					phoneBusy = true
					mu.Unlock()
					defer func() { mu.Lock(); phoneBusy = false; mu.Unlock() }()
				}
				width := 1080
				if transport == 3 {
					width = 640
				}
				if _, err = fmt.Fprintf(conn, "OKAYDisplay 0:\n mBaseDisplayInfo=DisplayInfo{real %d x 2400, rotation 0}\n mOverrideDisplayInfo=null\n", width); err != nil {
					t.Error(err)
					return
				}
				if transport == 1 {
					close(started)
					<-release
					if _, err = io.WriteString(conn, "display dump complete\n"); err != nil {
						t.Error(err)
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { unblock(); listener.Close(); workers.Wait() })
	client, err := adb.NewClient(listener.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	registry := New(ctx, nil, client, slog.New(slog.NewTextHandler(io.Discard, nil)), 3*time.Second)
	defer registry.Close()
	type result struct {
		geometry ScreenGeometry
		err      error
	}
	first := make(chan result, 1)
	go func() {
		geometry, err := registry.readDisplay(ctx, "same-phone", 1)
		first <- result{geometry, err}
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// Another phone must remain readable while this phone is still dumping.
	other, err := registry.readDisplay(ctx, "other-phone", 3)
	if err != nil || other.Width != 640 {
		t.Fatalf("another phone was blocked: geometry=%+v err=%v", other, err)
	}
	queued, stop := context.WithTimeout(ctx, 250*time.Millisecond)
	defer stop()
	if _, err = registry.readDisplay(queued, "same-phone", 2); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("same-phone wait did not respect cancellation: %v", err)
	}
	unblock()
	completed := <-first
	if completed.err != nil || completed.geometry.Width != 1080 {
		t.Fatalf("first display dump failed: geometry=%+v err=%v", completed.geometry, completed.err)
	}
	next, err := registry.readDisplay(ctx, "same-phone", 2)
	if err != nil || next.Width != 1080 {
		t.Fatalf("completed dump left phone busy: geometry=%+v err=%v", next, err)
	}
}

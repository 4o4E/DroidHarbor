package capture

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"droidharbor/internal/adb"
	"droidharbor/internal/leases"
	"droidharbor/internal/scrcpy"
	"droidharbor/internal/storage"
)

func TestRecoveryWaitsForOwnedCleanupHelper(t *testing.T) {
	for _, test := range []struct {
		name             string
		unknownIdentity  bool
		survivingProcess bool
		refuseStop       bool
		retiringOwner    bool
	}{
		{name: "helper finishes after parent"},
		{name: "unconfirmed helper keeps resource pending", unknownIdentity: true},
		{name: "owned survivor exits after termination", survivingProcess: true},
		{name: "termination failure keeps resource pending", survivingProcess: true, refuseStop: true},
		{name: "replacement transport reclaims retiring owner", retiringOwner: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "capture.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			loc := locator{Jar: "/data/local/tmp/droidharbor-" + strings.Repeat("a", 32) + ".jar", SCID: 1, Socket: "scrcpy_00000001", BootUUID: "11111111-2222-3333-4444-555555555555", PID: 17, StartTicks: 19}
			if _, err := db.Exec(`INSERT INTO devices(id,display_name,allocation_state,maintenance_requested) VALUES('device','phone','quarantined',1)`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO owned_resources(id,device_id,owner_kind,owner_id,boot_id,kind,locator,state,created_at) VALUES('capture','device','device-capture','capture','boot','scrcpy',?,'failed',0)`, mustJSON(loc)); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			var workers sync.WaitGroup
			var helperProbes atomic.Int32
			var helperAbsent, artifactRemoved atomic.Bool
			var processAlive atomic.Bool
			processAlive.Store(test.survivingProcess)
			workers.Add(1)
			go func() {
				defer workers.Done()
				for {
					conn, err := listener.Accept()
					if err != nil {
						return
					}
					func() {
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
							payload := make([]byte, int(n))
							_, err = io.ReadFull(conn, payload)
							return string(payload), err
						}
						service, err := readService()
						if err != nil {
							t.Error(err)
							return
						}
						if service != "host:transport-id:7" {
							t.Error("cleanup selected an unexpected transport")
							return
						}
						if _, err := io.WriteString(conn, "OKAY"); err != nil {
							t.Error(err)
							return
						}
						service, err = readService()
						if err != nil {
							t.Error(err)
							return
						}
						if !strings.HasPrefix(service, "shell,v2,raw:") {
							t.Error("cleanup opened a non-shell service")
							return
						}
						command := strings.TrimPrefix(service, "shell,v2,raw:")
						output := ""
						switch command {
						case processProbeCommand(loc):
							if processAlive.Load() {
								output = "MATCH"
							} else {
								output = "GONE"
							}
						case processStopCommand(loc):
							if test.refuseStop {
								output = "ERROR"
							} else {
								processAlive.Store(false)
								output = "SIGNALED"
							}
						case helperProbeCommand(loc.Jar):
							if processAlive.Load() {
								t.Error("helper cleanup began before capture exit")
								return
							}
							if test.unknownIdentity {
								output = "ERROR"
							} else if helperProbes.Add(1) == 1 {
								output = "FOUND"
							} else {
								output = "ABSENT"
								helperAbsent.Store(true)
							}
						default:
							if !strings.HasPrefix(command, "rm -f -- "+shellQuote(loc.Jar)+";") {
								t.Error("cleanup targeted an unexpected artifact")
								return
							}
							if !helperAbsent.Load() {
								t.Error("artifact removed before helper exit was confirmed")
								return
							}
							artifactRemoved.Store(true)
							output = "REMOVED"
						}
						if _, err := io.WriteString(conn, "OKAY"); err != nil {
							t.Error(err)
							return
						}
						var closeStdin [5]byte
						if _, err := io.ReadFull(conn, closeStdin[:]); err != nil {
							t.Error(err)
							return
						}
						if closeStdin != [5]byte{4, 0, 0, 0, 0} {
							t.Error("unexpected shell input during read-only cleanup probe")
							return
						}
						packet := make([]byte, 5+len(output)+6)
						packet[0] = 1
						binary.LittleEndian.PutUint32(packet[1:5], uint32(len(output)))
						copy(packet[5:], output)
						copy(packet[5+len(output):], []byte{3, 1, 0, 0, 0, 0})
						if _, err := conn.Write(packet); err != nil {
							t.Error(err)
						}
					}()
				}
			}()
			t.Cleanup(func() { listener.Close(); workers.Wait() })
			client, err := adb.NewClient(listener.Addr().String(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			link := leases.Link{DeviceID: "device", LinkID: "link", TransportID: 7, Generation: 3}
			manager := &Manager{db: db, adb: client, source: &testSource{link: link, available: true}, recovering: make(map[string]*recoveryFlight), logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			if test.retiringOwner {
				oldLink := link
				oldLink.TransportID--
				oldLink.Generation--
				streamCtx, stop := context.WithCancel(ctx)
				stop()
				started := make(chan struct{})
				close(started)
				stream := &Stream{
					manager: manager, id: "capture", deviceID: "device", link: oldLink, ctx: streamCtx, cancel: stop,
					started: started, workersDone: make(chan struct{}), done: make(chan struct{}),
					frames: make(chan scrcpy.Packet), messages: make(chan scrcpy.DeviceMessage),
				}
				stream.journaled.Store(true)
				manager.streams = map[string]*Stream{"device": stream}
				manager.resourceFailure = func(context.Context, string) error { return nil }
			}
			complete, recoveryErr := manager.Recover(ctx, "device")
			var state string
			if err := db.QueryRow(`SELECT state FROM owned_resources WHERE id='capture'`).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if test.unknownIdentity || test.refuseStop {
				if complete || recoveryErr == nil || artifactRemoved.Load() || state == "cleaned" {
					t.Fatalf("unconfirmed helper was released: complete=%v err=%v removed=%v state=%s", complete, recoveryErr, artifactRemoved.Load(), state)
				}
			} else if !complete || recoveryErr != nil || processAlive.Load() || !artifactRemoved.Load() || state != "cleaned" {
				t.Fatal(fmt.Sprintf("normal asynchronous helper failed recovery: complete=%v err=%v removed=%v state=%s", complete, recoveryErr, artifactRemoved.Load(), state))
			}
		})
	}
}

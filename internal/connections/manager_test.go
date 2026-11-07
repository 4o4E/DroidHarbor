package connections

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"droidharbor/internal/adb"
	"droidharbor/internal/identity"
	"droidharbor/internal/leases"
	"droidharbor/internal/storage"
)

type connectionSource struct {
	link    leases.Link
	ctx     context.Context
	cleanup func(context.Context, string) error
}

func (s *connectionSource) OnlineLinks() []leases.Link { return []leases.Link{s.link} }
func (s *connectionSource) HasLink(link leases.Link) bool {
	return s.ctx.Err() == nil && link == s.link
}
func (s *connectionSource) LinkContext(link leases.Link) (context.Context, error) {
	if !s.HasLink(link) {
		return nil, leases.ErrDeviceOffline
	}
	return s.ctx, nil
}
func (s *connectionSource) Recover(ctx context.Context, id string) (bool, error) {
	if err := s.cleanup(ctx, id); err != nil {
		return false, err
	}
	return s.ctx.Err() == nil, nil
}

type connectionFixture struct {
	manager   *Manager
	authority *leases.Manager
	db        *sql.DB
	lease     leases.Lease
	principal leases.Principal
}

func newConnectionFixture(t *testing.T, features func(net.Conn)) connectionFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "connections.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	caller := "dhc_" + identity.Secret()
	if _, err = db.Exec(`INSERT INTO caller_credentials(id,name,secret_hash,created_at) VALUES('caller','fixture',?,?)`, identity.Digest(caller), time.Now().UTC().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO devices(id,display_name,allocation_state,maintenance_requested) VALUES('device','fixture','available',0)`); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var worker sync.WaitGroup
	worker.Add(1)
	go func() {
		defer worker.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
			var prefix [4]byte
			_, err = io.ReadFull(conn, prefix[:])
			length, parseErr := strconv.ParseUint(string(prefix[:]), 16, 16)
			if err != nil || parseErr != nil {
				_ = conn.Close()
				continue
			}
			service := make([]byte, int(length))
			_, err = io.ReadFull(conn, service)
			if err == nil && string(service) == "host-transport-id:7:features" {
				if features != nil {
					features(conn)
				} else {
					payload := "shell_v2,cmd,sendrecv_v2"
					_, _ = fmt.Fprintf(conn, "OKAY%04x%s", len(payload), payload)
				}
			}
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); worker.Wait() })
	client, err := adb.NewClient(listener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	source := &connectionSource{ctx: ctx, link: leases.Link{DeviceID: "device", LinkID: "link", TransportID: 7, Generation: 1}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager := New(ctx, db, client, source, logger)
	source.cleanup = manager.Recover
	authority, err := leases.New(ctx, db, source, logger)
	if err != nil {
		_ = manager.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close(); _ = authority.Close() })
	secret := identity.Secret()
	lease, err := authority.Claim(ctx, caller, leases.ClaimRequest{TaskID: "fixture-task", TaskSecret: secret, IdempotencyKey: identity.Secret()})
	if err != nil {
		t.Fatal(err)
	}
	principal, err := authority.Authenticate(ctx, secret)
	if err != nil {
		t.Fatal(err)
	}
	return connectionFixture{manager, authority, db, lease, principal}
}

func connectionPublicKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var wire [524]byte
	binary.LittleEndian.PutUint32(wire[:4], 64)
	modulus := key.N.FillBytes(make([]byte, 256))
	for i, b := range modulus {
		wire[8+255-i] = b
	}
	radix32 := new(big.Int).Lsh(big.NewInt(1), 32)
	inverse := new(big.Int).ModInverse(key.N, radix32)
	inverse.Neg(inverse).Mod(inverse, radix32)
	binary.LittleEndian.PutUint32(wire[4:8], uint32(inverse.Uint64()))
	rr := new(big.Int).Lsh(big.NewInt(1), 4096)
	rr.Mod(rr, key.N)
	montgomery := rr.FillBytes(make([]byte, 256))
	for i, b := range montgomery {
		wire[264+255-i] = b
	}
	binary.LittleEndian.PutUint32(wire[520:], 65537)
	return base64.StdEncoding.EncodeToString(wire[:]) + " fixture"
}

func TestConnectionIdempotencyCannotReviveUsedTunnel(t *testing.T) {
	f := newConnectionFixture(t, nil)
	ctx := context.Background()
	key := identity.Secret()
	request := Request{ProtocolMajor: 1, ADBPublicKey: connectionPublicKey(t)}
	first, created, err := f.manager.Create(ctx, f.authority, f.principal, f.lease.ID, key, request)
	if err != nil || !created || first.Status != "pending" {
		t.Fatalf("create: %+v %v %v", first, created, err)
	}
	repeated, created, err := f.manager.Create(ctx, f.authority, f.principal, f.lease.ID, key, request)
	if err != nil || created || repeated.ConnectionID != first.ConnectionID {
		t.Fatalf("replay: %+v %v %v", repeated, created, err)
	}
	changed := request
	changed.ADBPublicKey = connectionPublicKey(t)
	if _, _, err = f.manager.Create(ctx, f.authority, f.principal, f.lease.ID, key, changed); !errors.Is(err, ErrIdempotency) {
		t.Fatalf("key substitution: %v", err)
	}
	if err = f.manager.Recover(ctx, f.lease.DeviceID); !errors.Is(err, leases.ErrConflict) {
		t.Fatalf("active lease was reclaimed: %v", err)
	}
	tunnel, err := f.manager.Begin(ctx, f.lease, first.ConnectionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.manager.Begin(ctx, f.lease, first.ConnectionID); !errors.Is(err, ErrUsed) {
		t.Fatalf("second transport reservation: %v", err)
	}
	if err = tunnel.Close(); err != nil {
		t.Fatal(err)
	}
	repeated, created, err = f.manager.Create(ctx, f.authority, f.principal, f.lease.ID, key, request)
	if err != nil || created || repeated.Status != "closed" {
		t.Fatalf("closed replay: %+v %v %v", repeated, created, err)
	}
	if _, err = f.manager.Begin(ctx, f.lease, first.ConnectionID); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed tunnel revived: %v", err)
	}
	current, err := f.authority.Get(ctx, f.principal, f.lease.ID)
	if err != nil || current.Status != leases.LeaseActive {
		t.Fatalf("disconnect ended lease: %+v %v", current, err)
	}
}

func TestReleaseJoinsCreationBeforeReturningDevice(t *testing.T) {
	entered := make(chan struct{})
	f := newConnectionFixture(t, func(conn net.Conn) {
		close(entered)
		var b [1]byte
		_, _ = conn.Read(b[:])
	})
	request := Request{ProtocolMajor: 1, ADBPublicKey: connectionPublicKey(t)}
	result := make(chan error, 1)
	go func() {
		_, _, err := f.manager.Create(context.Background(), f.authority, f.principal, f.lease.ID, identity.Secret(), request)
		result <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("creation did not reach ADB")
	}
	var state string
	if err := f.db.QueryRow(`SELECT state FROM owned_resources WHERE owner_id=? AND kind=?`, f.lease.TaskID, Kind).Scan(&state); err != nil || state != "pending" {
		t.Fatalf("external I/O preceded durable ownership: state=%q err=%v", state, err)
	}
	if _, err := f.authority.Release(context.Background(), f.principal, f.lease.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("revoked creation succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("revocation left creation blocked")
	}
	if err := f.db.QueryRow(`SELECT state FROM owned_resources WHERE owner_id=? AND kind=?`, f.lease.TaskID, Kind).Scan(&state); err != nil || state != "cleaned" {
		t.Fatalf("release left resource uncleaned: state=%q err=%v", state, err)
	}
	if err := f.db.QueryRow(`SELECT allocation_state FROM devices WHERE id=?`, f.lease.DeviceID).Scan(&state); err != nil || state != "available" {
		t.Fatalf("recovered healthy device not available: state=%q err=%v", state, err)
	}
}

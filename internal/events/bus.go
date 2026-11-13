package events

import (
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
)

const (
	MaxSubscriptions = 256
	QueueCapacity    = 32
)

var ErrClosed = errors.New("event bus closed")
var ErrCapacity = errors.New("event subscriber capacity reached")

type Change struct {
	EntityType string
	EntityID   string
	TaskID     string
	DeviceID   string
}

type Event struct {
	EventID       string `json:"eventId"`
	BootID        string `json:"bootId"`
	EntityVersion uint64 `json:"entityVersion"`
	EntityType    string `json:"entityType"`
	EntityID      string `json:"entityId"`
	TaskID        string `json:"-"`
	DeviceID      string `json:"-"`
}

type Scope struct {
	Admin  bool
	TaskID string
}

type Subscription struct {
	C          <-chan Event
	Done       <-chan struct{}
	bus        *Bus
	subscriber *subscriber
	once       sync.Once
}

type subscriber struct {
	ch    chan Event
	done  chan struct{}
	scope Scope
}

type Bus struct {
	mu         sync.Mutex
	bootID     string
	bootPrefix string
	sequence   uint64
	subs       map[*subscriber]struct{}
	closed     bool
}

func New(bootID string) *Bus {
	if bootID == "" {
		panic("events: empty boot ID")
	}
	return &Bus{bootID: bootID, bootPrefix: base64.RawURLEncoding.EncodeToString([]byte(bootID)), subs: make(map[*subscriber]struct{})}
}

func (b *Bus) Publish(change Change) (Event, error) {
	if change.EntityType != "device" && change.EntityType != "device-runtime" && change.EntityType != "lease" && change.EntityType != "caller" {
		return Event{}, fmt.Errorf("invalid entity type")
	}
	if change.EntityID == "" {
		return Event{}, fmt.Errorf("empty entity ID")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return Event{}, ErrClosed
	}
	b.sequence++
	e := b.event(change)
	for s := range b.subs {
		if !permitted(s.scope, e) {
			continue
		}
		select {
		case s.ch <- e:
		default:
			b.removeLocked(s)
		}
	}
	return e, nil
}

func (b *Bus) event(change Change) Event {
	return Event{EventID: b.bootPrefix + fmt.Sprintf(":%d", b.sequence), BootID: b.bootID, EntityVersion: b.sequence, EntityType: change.EntityType, EntityID: change.EntityID, TaskID: change.TaskID, DeviceID: change.DeviceID}
}

func permitted(scope Scope, e Event) bool {
	if e.EntityType == "resync" {
		return true
	}
	if scope.Admin {
		return true
	}
	if scope.TaskID == "" {
		return false
	}
	if e.EntityType == "lease" {
		return e.TaskID == scope.TaskID
	}
	if e.EntityType == "device" {
		return e.TaskID == scope.TaskID
	}
	return false
}

func (b *Bus) Subscribe(scope Scope) (*Subscription, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, ErrClosed
	}
	if len(b.subs) >= MaxSubscriptions {
		return nil, ErrCapacity
	}
	s := &subscriber{ch: make(chan Event, QueueCapacity), done: make(chan struct{}), scope: scope}
	b.subs[s] = struct{}{}
	s.ch <- b.event(Change{EntityType: "resync", EntityID: "snapshot"})
	return &Subscription{C: s.ch, Done: s.done, bus: b, subscriber: s}, nil
}

func (s *Subscription) Close() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		s.bus.mu.Lock()
		s.bus.removeLocked(s.subscriber)
		s.bus.mu.Unlock()
	})
}

func (b *Bus) removeLocked(s *subscriber) {
	if _, ok := b.subs[s]; ok {
		delete(b.subs, s)
		close(s.ch)
		close(s.done)
	}
}

func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for s := range b.subs {
		b.removeLocked(s)
	}
}

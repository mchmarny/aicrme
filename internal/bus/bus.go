package bus

import (
	"sync"
	"time"
)

// subscriberBuffer is the per-subscriber queue depth. A subscriber that falls
// this far behind is dropped rather than allowed to block Publish; the browser
// reconnects with Last-Event-ID and replays from the ring.
const subscriberBuffer = 256

// Bus is a fan-out hub with a bounded replay ring. Safe for concurrent use.
type Bus struct {
	mu       sync.RWMutex
	nextID   uint64
	ring     []Event
	capacity int
	subs     map[int]chan Event
	nextSub  int
	now      func() time.Time
}

// New returns a Bus retaining the most recent capacity events for replay.
func New(capacity int) *Bus {
	if capacity < 1 {
		capacity = 1
	}
	return &Bus{
		ring:     make([]Event, 0, capacity),
		capacity: capacity,
		subs:     make(map[int]chan Event),
		now:      time.Now,
	}
}

// Publish stamps e with the next ID and current time, retains it for replay,
// and delivers it to every live subscriber. A subscriber whose buffer is full
// is skipped, never waited on. The send loop runs under the same lock that
// guards subscriber close so a channel can never be written to and closed
// concurrently. Returns the stamped event.
func (b *Bus) Publish(e Event) Event {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.nextID++
	e.ID = b.nextID
	if e.At.IsZero() {
		e.At = b.now().UTC()
	}
	if e.Level == "" {
		e.Level = LevelInfo
	}
	if len(b.ring) == b.capacity {
		b.ring = append(b.ring[:0], b.ring[1:]...)
	}
	b.ring = append(b.ring, e)

	for _, ch := range b.subs {
		select {
		case ch <- e:
		default: // slow subscriber: drop, it will replay on reconnect
		}
	}
	return e
}

// Replay returns retained events with ID greater than since, oldest first.
func (b *Bus) Replay(since uint64) []Event {
	b.mu.RLock()
	defer b.mu.RUnlock()

	var out []Event
	for _, e := range b.ring {
		if e.ID > since {
			out = append(out, e)
		}
	}
	return out
}

// Subscribe returns a channel that first yields retained events newer than
// since, then streams live events. The returned func unsubscribes and closes
// the channel; it is safe to call more than once.
func (b *Bus) Subscribe(since uint64) (<-chan Event, func()) {
	b.mu.Lock()
	id := b.nextSub
	b.nextSub++
	ch := make(chan Event, subscriberBuffer)
	backlog := make([]Event, 0, len(b.ring))
	for _, e := range b.ring {
		if e.ID > since {
			backlog = append(backlog, e)
		}
	}
	b.subs[id] = ch
	b.mu.Unlock()

	for _, e := range backlog {
		select {
		case ch <- e:
		default:
		}
	}

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			delete(b.subs, id)
			close(ch)
		})
	}
}

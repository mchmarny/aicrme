package bus

import (
	"sync"
	"time"
)

// subscriberBuffer is the live-event queue depth added on top of a new
// subscriber's full backlog. A subscriber that falls this far behind on live
// events is dropped rather than allowed to block Publish; the browser
// reconnects with Last-Event-ID and replays from the ring.
const subscriberBuffer = 256

// Bus is a fan-out hub with a bounded replay ring. Safe for concurrent use.
type Bus struct {
	mu       sync.RWMutex
	nextID   uint64
	ring     []Event // fixed-size circular buffer, length == capacity
	head     int     // index of the oldest retained event
	count    int     // number of retained events, 0 <= count <= capacity
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
		ring:     make([]Event, capacity),
		capacity: capacity,
		subs:     make(map[int]chan Event),
		now:      time.Now,
	}
}

// Publish stamps e with the next ID and current time, retains it for replay,
// and delivers it to every live subscriber. A subscriber whose buffer is full
// is skipped, never waited on. The whole critical section, including the
// fan-out send, runs under b.mu so a channel can never be written to and
// closed concurrently, and so a subscriber can never be registered mid-send.
// Returns the stamped event.
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
	b.retain(e)

	for _, ch := range b.subs {
		select {
		case ch <- e:
		default: // slow subscriber: drop, it will replay on reconnect
		}
	}
	return e
}

// retain appends e to the circular buffer, evicting the oldest retained
// event in O(1) once the ring is full. Callers must hold b.mu.
func (b *Bus) retain(e Event) {
	if b.count < b.capacity {
		b.ring[(b.head+b.count)%b.capacity] = e
		b.count++
		return
	}
	b.ring[b.head] = e
	b.head = (b.head + 1) % b.capacity
}

// since returns retained events with ID greater than since, oldest first.
// Callers must hold b.mu for reading.
func (b *Bus) since(since uint64) []Event {
	var out []Event
	for i := 0; i < b.count; i++ {
		e := b.ring[(b.head+i)%b.capacity]
		if e.ID > since {
			out = append(out, e)
		}
	}
	return out
}

// Replay returns retained events with ID greater than since, oldest first.
func (b *Bus) Replay(since uint64) []Event {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return b.since(since)
}

// Subscribe returns a channel that first yields retained events newer than
// since, then streams live events. The channel is sized to hold the entire
// backlog plus subscriberBuffer live events, so a reconnecting browser never
// silently loses history to a fixed-size queue. The backlog is queued and
// the subscriber registered for live fan-out in a single b.mu hold, with
// registration last, so a concurrent Publish can never deliver a live event
// ahead of older backlog on the same channel. The returned func unsubscribes
// and closes the channel; it is safe to call more than once.
func (b *Bus) Subscribe(since uint64) (<-chan Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()

	backlog := b.since(since)
	ch := make(chan Event, len(backlog)+subscriberBuffer)
	for _, e := range backlog {
		ch <- e // capacity guarantees this never blocks
	}

	id := b.nextSub
	b.nextSub++
	b.subs[id] = ch

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

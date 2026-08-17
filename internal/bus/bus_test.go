package bus_test

import (
	"sync"
	"testing"
	"time"

	"github.com/mchmarny/aicrme/internal/bus"
)

func TestPublishAssignsMonotonicIDs(t *testing.T) {
	b := bus.New(8)
	first := b.Publish(bus.Event{Message: "a"})
	second := b.Publish(bus.Event{Message: "b"})

	if first.ID != 1 {
		t.Errorf("first ID = %d, want 1", first.ID)
	}
	if second.ID != 2 {
		t.Errorf("second ID = %d, want 2", second.ID)
	}
	if first.At.IsZero() {
		t.Error("Publish did not stamp At")
	}
}

func TestReplay(t *testing.T) {
	tests := []struct {
		name     string
		capacity int
		publish  int
		since    uint64
		wantIDs  []uint64
	}{
		{name: "from start", capacity: 8, publish: 3, since: 0, wantIDs: []uint64{1, 2, 3}},
		{name: "after id 2", capacity: 8, publish: 3, since: 2, wantIDs: []uint64{3}},
		{name: "caught up", capacity: 8, publish: 3, since: 3, wantIDs: nil},
		{name: "ring evicts oldest", capacity: 2, publish: 4, since: 0, wantIDs: []uint64{3, 4}},
		{name: "ring wraps multiple times", capacity: 3, publish: 10, since: 0, wantIDs: []uint64{8, 9, 10}},
		// since=99 exceeds nextID (2): an impossible cursor from a previous
		// process, so it replays everything rather than filtering it all out.
		// See TestSinceAboveNextIDReplaysEverything for the focused case.
		{name: "since above nextID replays everything", capacity: 8, publish: 2, since: 99, wantIDs: []uint64{1, 2}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := bus.New(tc.capacity)
			for i := 0; i < tc.publish; i++ {
				b.Publish(bus.Event{Message: "x"})
			}
			got := b.Replay(tc.since)
			if len(got) != len(tc.wantIDs) {
				t.Fatalf("Replay(%d) returned %d events, want %d", tc.since, len(got), len(tc.wantIDs))
			}
			for i, want := range tc.wantIDs {
				if got[i].ID != want {
					t.Errorf("event[%d].ID = %d, want %d", i, got[i].ID, want)
				}
			}
		})
	}
}

func TestSubscribeReplaysThenStreams(t *testing.T) {
	b := bus.New(8)
	b.Publish(bus.Event{Message: "before"})

	ch, cancel := b.Subscribe(0)
	defer cancel()

	if got := <-ch; got.Message != "before" {
		t.Fatalf("first event = %q, want %q", got.Message, "before")
	}

	b.Publish(bus.Event{Message: "after"})
	select {
	case got := <-ch:
		if got.Message != "after" {
			t.Errorf("second event = %q, want %q", got.Message, "after")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for live event")
	}
}

func TestSlowSubscriberDoesNotBlockPublish(t *testing.T) {
	b := bus.New(4)
	_, cancel := b.Subscribe(0) // never drained
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			b.Publish(bus.Event{Message: "flood"})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a slow subscriber")
	}
}

func TestConcurrentSubscribeUnsubscribe(t *testing.T) { //nolint:unparam // t is required by the testing.T signature; -race proves the churn is safe
	b := bus.New(16)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, cancel := b.Subscribe(0)
			b.Publish(bus.Event{Message: "churn"})
			<-ch
			cancel()
		}()
	}
	wg.Wait()
}

// TestSubscribeBacklogExceedsBuffer proves a reconnecting subscriber sees the
// whole retained run even when the backlog is far larger than the live
// per-subscriber queue depth: production runs a Bus sized in the thousands,
// and Subscribe must never silently drop backlog to fit a small fixed
// channel.
func TestSubscribeBacklogExceedsBuffer(t *testing.T) {
	const n = 1000
	b := bus.New(n)
	for i := 0; i < n; i++ {
		b.Publish(bus.Event{Message: "x"})
	}

	ch, cancel := b.Subscribe(0)
	defer cancel()

	got := make([]bus.Event, 0, n)
	for i := 0; i < n; i++ {
		select {
		case e := <-ch:
			got = append(got, e)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out after %d events, want %d", len(got), n)
		}
	}

	if len(got) != n {
		t.Fatalf("got %d backlog events, want %d", len(got), n)
	}
	for i, e := range got {
		want := uint64(i + 1)
		if e.ID != want {
			t.Errorf("event[%d].ID = %d, want %d", i, e.ID, want)
		}
	}
}

// TestSubscribeOrderingUnderConcurrentPublish proves backlog delivery and
// live fan-out never interleave out of order: Subscribe must register in
// the fan-out set only after its entire backlog is already queued, so a
// concurrent Publish can never land a newer event ahead of older backlog in
// the same channel.
func TestSubscribeOrderingUnderConcurrentPublish(t *testing.T) {
	const backlogN, liveN = 50, 50
	b := bus.New(128)
	for i := 0; i < backlogN; i++ {
		b.Publish(bus.Event{Message: "backlog"})
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < liveN; i++ {
			b.Publish(bus.Event{Message: "live"})
		}
	}()

	close(start)
	ch, cancel := b.Subscribe(0)
	defer cancel()

	got := make([]uint64, 0, backlogN+liveN)
	for i := 0; i < backlogN+liveN; i++ {
		select {
		case e := <-ch:
			got = append(got, e.ID)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out after %d events, want %d", len(got), backlogN+liveN)
		}
	}
	wg.Wait()

	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("events out of order at index %d: got[%d]=%d, got[%d]=%d", i, i-1, got[i-1], i, got[i])
		}
	}
}

// TestEpochDiffersAcrossBuses proves the epoch is generated per Bus instance,
// not derived from something two processes started in the same second could
// share (a timestamp). The SPA's restart detection depends on this: an epoch
// that collided across a restart would look like no restart happened at all.
func TestEpochDiffersAcrossBuses(t *testing.T) {
	b1 := bus.New(8)
	b2 := bus.New(8)

	if b1.Epoch() == "" {
		t.Fatal("Epoch() returned an empty string")
	}
	if b1.Epoch() == b2.Epoch() {
		t.Fatalf("two independent Bus instances share an epoch: %q", b1.Epoch())
	}
}

// TestSinceBelowNextIDReplaysTheRemainder is the ordinary reconnect case: a
// cursor from earlier in the same process's run replays only what came
// after it.
func TestSinceBelowNextIDReplaysTheRemainder(t *testing.T) {
	b := bus.New(8)
	b.Publish(bus.Event{Message: "one"})
	b.Publish(bus.Event{Message: "two"})
	b.Publish(bus.Event{Message: "three"})

	got := b.Replay(1) // nextID is 3; since (1) is below it
	wantIDs := []uint64{2, 3}
	if len(got) != len(wantIDs) {
		t.Fatalf("Replay(1) returned %d events, want %d", len(got), len(wantIDs))
	}
	for i, want := range wantIDs {
		if got[i].ID != want {
			t.Errorf("event[%d].ID = %d, want %d", i, got[i].ID, want)
		}
	}
}

// TestSinceEqualToNextIDReplaysNothing is the boundary: a client fully
// caught up with the process that issued its cursor gets an empty replay,
// not the whole backlog.
func TestSinceEqualToNextIDReplaysNothing(t *testing.T) {
	b := bus.New(8)
	b.Publish(bus.Event{Message: "one"})
	b.Publish(bus.Event{Message: "two"})

	got := b.Replay(2) // nextID is 2; since == nextID
	if len(got) != 0 {
		t.Fatalf("Replay(2) returned %d events, want 0", len(got))
	}
}

// TestSinceAboveNextIDReplaysEverything is the case a restart produces: the
// client's cursor was issued by a process that no longer exists, so it sits
// above the new process's nextID -- a cursor no live process could have
// handed out. Filtering everything below it (the pre-fix behavior) is the
// silent-stream bug; replaying from 0 is the fix.
func TestSinceAboveNextIDReplaysEverything(t *testing.T) {
	b := bus.New(8)
	b.Publish(bus.Event{Message: "one"})
	b.Publish(bus.Event{Message: "two"})

	got := b.Replay(99) // nextID is 2; since (99) is above it
	wantIDs := []uint64{1, 2}
	if len(got) != len(wantIDs) {
		t.Fatalf("Replay(99) returned %d events, want %d", len(got), len(wantIDs))
	}
	for i, want := range wantIDs {
		if got[i].ID != want {
			t.Errorf("event[%d].ID = %d, want %d", i, got[i].ID, want)
		}
	}
}

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
		{name: "since beyond head", capacity: 8, publish: 2, since: 99, wantIDs: nil},
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

package observer

import (
	"sync"
	"testing"
	"time"

	"github.com/mchmarny/aicrme/internal/bus"
)

// collector records what actually reached the bus.
type collector struct {
	mu   sync.Mutex
	sent []bus.ClusterData
}

func (c *collector) emit(_, _ string, cd bus.ClusterData) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, cd)
}

func (c *collector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sent)
}

func trouble(uid, reason string) bus.ClusterData {
	return bus.ClusterData{UID: uid, Reason: reason, Severity: bus.SeverityWarn, Kind: "Pod", Name: "p"}
}

func resolvedTrouble(uid, reason string) bus.ClusterData {
	cd := trouble(uid, reason)
	cd.Resolved = true
	return cd
}

// THE POINT. On one real EKS run, 297 of 373 arise/resolve pairs resolved
// within two seconds -- FailedCreatePodSandBox while containerd restarted,
// NodeNotReady while a node rebooted. Neither half is actionable, and together
// they were two-thirds of the run log.
func TestDebounceDropsBothHalvesOfASelfHealingPair(t *testing.T) {
	d := newDebouncer(time.Hour) // long: the pair must resolve well inside it
	c := &collector{}

	d.submit("ns", "arose", trouble("u-1", "FailedCreatePodSandBox"), c.emit)
	d.submit("ns", "resolved", resolvedTrouble("u-1", "FailedCreatePodSandBox"), c.emit)

	if got := c.count(); got != 0 {
		t.Errorf("published %d events, want 0 -- the pair corrected itself inside the window: %+v", got, c.sent)
	}
}

// A DELAY, not a filter. Anything that outlives the window is published --
// the skyhook CrashLoopBackOff that ran for twenty minutes must be unaffected.
func TestDebouncePublishesTroubleThatOutlivesTheWindow(t *testing.T) {
	d := newDebouncer(10 * time.Millisecond)
	c := &collector{}

	d.submit("ns", "arose", trouble("u-2", "CrashLoopBackOff"), c.emit)

	deadline := time.Now().Add(2 * time.Second)
	for c.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := c.count(); got != 1 {
		t.Fatalf("published %d events, want 1 -- a persistent condition must still arrive", got)
	}
}

// A resolution with no pending arise is a real state change and passes through:
// it is how a condition raised before the window existed gets cleared.
func TestDebouncePublishesAResolutionWithNoPendingArise(t *testing.T) {
	d := newDebouncer(time.Hour)
	c := &collector{}

	d.submit("ns", "resolved", resolvedTrouble("u-3", "Unhealthy"), c.emit)

	if got := c.count(); got != 1 {
		t.Errorf("published %d events, want 1", got)
	}
}

// Rollout summaries carry a UID and Reason too. Holding them would swallow the
// "1/5 ... 5/5 ready" narration an operator watches an install by, since each
// step would replace the pending one and only the last would survive.
func TestDebouncePassesInfoSeverityStraightThrough(t *testing.T) {
	d := newDebouncer(time.Hour)
	c := &collector{}

	for i := range 3 {
		cd := bus.ClusterData{UID: "ds-1", Reason: "RolloutProgress", Severity: bus.SeverityInfo, Ready: int32(i)}
		d.submit("ns", "progress", cd, c.emit)
	}

	if got := c.count(); got != 3 {
		t.Errorf("published %d rollout events, want all 3 -- progress must not be held", got)
	}
}

// Anything without an identity cannot be paired with a resolution, so there is
// nothing to wait for.
func TestDebouncePassesUnidentifiedDataStraightThrough(t *testing.T) {
	d := newDebouncer(time.Hour)
	c := &collector{}

	d.submit("ns", "gpu allocatable 0 -> 8", bus.ClusterData{Kind: "Node", Name: "n1"}, c.emit)

	if got := c.count(); got != 1 {
		t.Errorf("published %d events, want 1", got)
	}
}

// A re-arise replaces rather than stacks: same condition, same object, and two
// pending entries would publish it twice.
func TestDebounceDoesNotStackRepeatedArises(t *testing.T) {
	d := newDebouncer(10 * time.Millisecond)
	c := &collector{}

	for range 5 {
		d.submit("ns", "arose", trouble("u-4", "ImagePullBackOff"), c.emit)
	}

	time.Sleep(200 * time.Millisecond)
	if got := c.count(); got != 1 {
		t.Errorf("published %d events, want exactly 1", got)
	}
}

// A condition still inside its window when the run ends belongs to that run.
// Publishing it afterwards would attach it to whatever the console shows next.
func TestDebounceStopDropsWhatIsStillPending(t *testing.T) {
	d := newDebouncer(10 * time.Millisecond)
	c := &collector{}

	d.submit("ns", "arose", trouble("u-5", "Unhealthy"), c.emit)
	d.stop()

	time.Sleep(200 * time.Millisecond)
	if got := c.count(); got != 0 {
		t.Errorf("published %d events after stop, want 0", got)
	}
}

// After stop nothing new is accepted either.
func TestDebounceIgnoresSubmissionsAfterStop(t *testing.T) {
	d := newDebouncer(0) // zero delay: would publish inline if not stopped
	c := &collector{}

	d.stop()
	d.submit("ns", "arose", trouble("u-6", "Unhealthy"), c.emit)

	if got := c.count(); got != 0 {
		t.Errorf("published %d events after stop, want 0", got)
	}
}

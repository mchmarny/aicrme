package observer

import (
	"sync"
	"time"

	"github.com/mchmarny/aicrme/internal/bus"
)

// conditionDebounce is how long a newly observed condition waits before it
// reaches the timeline.
//
// WHY THIS EXISTS, with the measurement that produced it
// One real run on EKS published 903 events, of which 845 (93%) were cluster
// conditions. Of 373 arise/resolve pairs, 297 -- 79% -- resolved within two
// seconds: FailedCreatePodSandBox while containerd restarted under the NVIDIA
// toolkit, NodeNotReady while a node rebooted, DNSConfigForming on every pod
// GKE starts. Roughly two-thirds of the entire log was churn that had already
// corrected itself before anyone could read it, and it buried the six events
// that mattered -- the validation narration.
//
// Two seconds is chosen from that distribution, not from taste. It is long
// enough to absorb the self-healing majority and short enough that a condition
// an operator could actually act on still appears while it is still true.
//
// This is a DELAY, not a filter. Anything that outlives the window is
// published unchanged, with the time it was observed rather than the time it
// was released -- so a persistent problem reads exactly as it did before. The
// skyhook CrashLoopBackOff that ran for twenty minutes on that same cluster is
// unaffected.
const conditionDebounce = 2 * time.Second

// defaultDebounce is what New gives a new Observer. A variable rather than the
// constant so the package's own tests can disable the window wholesale (see
// TestMain): almost every test here asserts that an event WAS published, and a
// two-second wait per assertion is not a test suite. The window's own
// behavior is covered directly in debounce_test.go, which builds debouncers
// with explicit delays and does not depend on this.
var defaultDebounce = conditionDebounce

// conditionKey matches bus.ClusterData's identity: same UID and same Reason.
// Deliberately the same pair clusterConditionSupersedes uses, so a resolution
// cancels exactly the arise it resolves and nothing else.
type conditionKey struct {
	uid    string
	reason string
}

// pendingCondition is an arise waiting out the window.
type pendingCondition struct {
	ns    string
	msg   string
	cd    bus.ClusterData
	timer *time.Timer
}

// debouncer holds the in-flight arises.
type debouncer struct {
	mu      sync.Mutex
	delay   time.Duration
	pending map[conditionKey]*pendingCondition
	// stopped makes teardown final: a timer that fires after the observer is
	// gone must not publish into a run that has ended.
	stopped bool
}

func newDebouncer(delay time.Duration) *debouncer {
	return &debouncer{delay: delay, pending: make(map[conditionKey]*pendingCondition)}
}

// submit decides what happens to one condition. emit is called for anything
// that should actually reach the bus, possibly later, on a timer goroutine.
func (d *debouncer) submit(ns, msg string, cd bus.ClusterData, emit func(string, string, bus.ClusterData)) {
	// Only TROUBLE is debounced. Rollout summaries carry a UID and Reason too
	// (RolloutProgress, severity info), and holding those would swallow the
	// "1/5 ... 3/5 ... 5/5 ready" narration an operator watches an install by:
	// each step replaces the pending one, so only the last would ever appear.
	// Severity is the line between "something is wrong" and "something is
	// happening", and it is the wrong half that self-heals in under two
	// seconds.
	// Stopped is checked FIRST, before any passthrough. After teardown this
	// publishes nothing at all -- an event belonging to a finished run must not
	// reach the console showing the next one, and that has to hold for the
	// undebounced kinds too.
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		return
	}

	// A zero delay disables the window and publishes inline. Tests rely on that
	// for determinism -- a two-second wait per assertion is not a test suite.
	if d.delay <= 0 || cd.UID == "" || cd.Reason == "" || cd.Severity == bus.SeverityInfo {
		d.mu.Unlock()
		emit(ns, msg, cd)
		return
	}

	k := conditionKey{cd.UID, cd.Reason}
	p, waiting := d.pending[k]

	if cd.Resolved {
		// The whole point: if the arise is still in the window, the pair
		// never happened as far as the operator is concerned.
		if waiting {
			p.timer.Stop()
			delete(d.pending, k)
			d.mu.Unlock()
			return
		}
		d.mu.Unlock()
		emit(ns, msg, cd)
		return
	}

	// A re-arise while one is already pending replaces it rather than
	// stacking: it is the same condition on the same object, and two entries
	// would publish it twice.
	if waiting {
		p.timer.Stop()
	}
	np := &pendingCondition{ns: ns, msg: msg, cd: cd}
	np.timer = time.AfterFunc(d.delay, func() { d.release(k, emit) })
	d.pending[k] = np
	d.mu.Unlock()
}

// release publishes an arise that outlived the window.
func (d *debouncer) release(k conditionKey, emit func(string, string, bus.ClusterData)) {
	d.mu.Lock()
	p, ok := d.pending[k]
	if ok {
		delete(d.pending, k)
	}
	stopped := d.stopped
	d.mu.Unlock()
	if ok && !stopped {
		emit(p.ns, p.msg, p.cd)
	}
}

// stop drops everything in flight. Called when the observer's stop channel
// closes: a condition still inside its window at teardown belongs to a run
// that is over, and publishing it afterwards would attach it to whatever the
// console shows next.
func (d *debouncer) stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stopped = true
	for k, p := range d.pending {
		p.timer.Stop()
		delete(d.pending, k)
	}
}

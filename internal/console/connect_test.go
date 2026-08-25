package console

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

const twoContextKubeconfig = `apiVersion: v1
kind: Config
current-context: alpha
clusters:
- name: alpha-cluster
  cluster: {server: https://alpha.example:6443}
- name: beta-cluster
  cluster: {server: https://beta.example:6443}
contexts:
- name: alpha
  context: {cluster: alpha-cluster, user: alpha-user}
- name: beta
  context: {cluster: beta-cluster, user: beta-user}
users:
- name: alpha-user
  user: {token: alpha-token}
- name: beta-user
  user: {token: beta-token}
`

func writeKubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(twoContextKubeconfig), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}

// fakeProber answers the one cluster round-trip Connect makes, so the
// connector's state machine can be tested without a cluster or a fake
// clientset for a call that reads two scalars.
type fakeProber struct {
	version string
	nodes   int
	err     error
}

func (p fakeProber) probe(context.Context, kubernetes.Interface) (string, int, error) {
	if p.err != nil {
		return "", 0, p.err
	}
	version := p.version
	if version == "" {
		version = "v1.34.0"
	}
	return version, p.nodes, nil
}

const testClusterUID = "11111111-2222-3333-4444-555555555555"

// kubeSystem is the namespace every connect reads to learn the cluster's
// identity. A connector without it cannot connect at all, which is the point
// -- see dial.
func kubeSystem(uid string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID(uid)}}
}

// newTestConnector builds a connector over the two-context kubeconfig whose
// clientset is a fake, so dial's real path -- including the identity read --
// runs without a cluster. Pass objects to control what that cluster contains.
func newTestConnector(t *testing.T, p prober, objects ...runtime.Object) *connector {
	t.Helper()
	if len(objects) == 0 {
		objects = []runtime.Object{kubeSystem(testClusterUID)}
	}
	kube := fake.NewClientset(objects...)
	c := newConnector(writeKubeconfig(t), p)
	c.newKube = func(*rest.Config) (kubernetes.Interface, error) { return kube, nil }
	return c
}

func TestListContextsReadsNamesServersAndCurrent(t *testing.T) {
	got, err := listContexts(writeKubeconfig(t))
	if err != nil {
		t.Fatalf("listContexts() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("listContexts() returned %d contexts, want 2", len(got))
	}
	byName := map[string]ContextInfo{}
	for _, c := range got {
		byName[c.Name] = c
	}
	if byName["alpha"].Server != "https://alpha.example:6443" {
		t.Errorf("alpha server = %q", byName["alpha"].Server)
	}
	if !byName["alpha"].Current {
		t.Error("alpha is the kubeconfig's current-context and was not marked current")
	}
	if byName["beta"].Current {
		t.Error("beta was marked current")
	}
}

// The order is the Connect screen's order, and a map range is not an order.
// Two loads of the same file that list the same contexts differently is a
// list that moves under the operator's cursor between renders.
func TestListContextsSortsByName(t *testing.T) {
	got, err := listContexts(writeKubeconfig(t))
	if err != nil {
		t.Fatalf("listContexts() error = %v", err)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Name > got[i].Name {
			t.Fatalf("listContexts() returned %q before %q, want name order", got[i-1].Name, got[i].Name)
		}
	}
}

// Listing contexts must not touch a cluster: the operator is choosing which
// one to talk to, and two of the three servers in a typical kubeconfig are
// unreachable from wherever they happen to be sitting.
func TestListContextsMakesNoClusterContact(t *testing.T) {
	// Every server above points at a name that does not resolve. A call that
	// dialed would take the resolver timeout; one that only reads the file
	// returns immediately.
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := listContexts(writeKubeconfig(t)); err != nil {
			t.Errorf("listContexts() error = %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("listContexts blocked -- it is contacting a cluster")
	}
}

// Connect is single-assignment. net/http serves every request on its own
// goroutine, and connect mutates process-global state, builds the clientset
// every step reads, and selects the run directory. Two of them interleaving
// is a torn connection, not a lost race.
func TestConcurrentConnectYieldsExactlyOneWinner(t *testing.T) {
	c := newTestConnector(t, fakeProber{})

	const callers = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners int
	)
	wg.Add(callers)
	for range callers {
		go func() {
			defer wg.Done()
			if _, err := c.Connect(context.Background(), "alpha"); err == nil {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if winners != 1 {
		t.Errorf("%d callers connected successfully, want exactly 1", winners)
	}
	if c.State() != stateConnected {
		t.Errorf("State() = %v, want stateConnected", c.State())
	}
}

func TestConnectAfterConnectIsRefused(t *testing.T) {
	c := newTestConnector(t, fakeProber{})
	if _, err := c.Connect(context.Background(), "alpha"); err != nil {
		t.Fatalf("first Connect() error = %v", err)
	}
	if _, err := c.Connect(context.Background(), "beta"); err == nil {
		t.Fatal("a second Connect succeeded -- switching clusters in-session is prohibited")
	}
}

func TestConnectReportsTheContextItReached(t *testing.T) {
	c := newTestConnector(t, fakeProber{version: "v1.33.4", nodes: 3})

	info, err := c.Connect(context.Background(), "beta")
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	if info.Context != "beta" {
		t.Errorf("Context = %q, want the context that was asked for", info.Context)
	}
	if info.Server != "https://beta.example:6443" {
		t.Errorf("Server = %q, want beta's server -- the connect used a different context than it reported", info.Server)
	}
	if info.Version != "v1.33.4" || info.NodeCount != 3 {
		t.Errorf("Version/NodeCount = %q/%d, want the probe's answer", info.Version, info.NodeCount)
	}
}

// A failed connect must leave the connector disconnected rather than stuck in
// connecting: a wrong context or a sleeping VPN is the ordinary case, and the
// operator has to be able to pick again without restarting the binary.
func TestFailedConnectLeavesTheConnectorReusable(t *testing.T) {
	c := newTestConnector(t, fakeProber{err: context.DeadlineExceeded})

	if _, err := c.Connect(context.Background(), "alpha"); err == nil {
		t.Fatal("Connect() succeeded against a prober that failed")
	}
	if c.State() != stateDisconnected {
		t.Fatalf("State() = %v after a failed connect, want stateDisconnected", c.State())
	}
	if _, _, ok := c.Cluster(); ok {
		t.Error("Cluster() reports a connection after a failed connect")
	}

	c.prober = fakeProber{}
	if _, err := c.Connect(context.Background(), "alpha"); err != nil {
		t.Fatalf("second Connect() after a failure error = %v", err)
	}
}

// An unknown context is refused by clientcmd before anything is dialed, and
// the error has to name it -- the operator typed it, or --context carried a
// stale name from a kubeconfig they have since edited.
func TestConnectToAnUnknownContextNamesIt(t *testing.T) {
	c := newTestConnector(t, fakeProber{})

	_, err := c.Connect(context.Background(), "gamma")
	if err == nil {
		t.Fatal("Connect() to a context that does not exist succeeded")
	}
	if !strings.Contains(err.Error(), "gamma") {
		t.Errorf("error = %v, want it to name the context that was not found", err)
	}
	if c.State() != stateDisconnected {
		t.Errorf("State() = %v, want stateDisconnected", c.State())
	}
}

// connectWith connects against a supplied clientset, so the identity read
// dial makes is exercised for real against a fake API server rather than
// stubbed out alongside the probe.
func connectWith(t *testing.T, objects ...runtime.Object) (ClusterInfo, error) {
	t.Helper()
	return newTestConnector(t, fakeProber{}, objects...).Connect(context.Background(), "alpha")
}

func TestConnectRecordsTheKubeSystemUID(t *testing.T) {
	info, err := connectWith(t, kubeSystem(testClusterUID))
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	if info.UID != testClusterUID {
		t.Errorf("UID = %q, want the kube-system UID", info.UID)
	}
}

// A cluster whose identity cannot be read is not a cluster this console can
// key a run directory on, so the connect fails rather than proceeding with an
// empty UID -- which would file every such cluster's runs in one directory.
func TestConnectFailsWhenTheIdentityCannotBeRead(t *testing.T) {
	// Built directly rather than through newTestConnector: this is the one
	// case that needs a cluster with no kube-system in it, and the helper
	// supplies one by default.
	c := newConnector(writeKubeconfig(t), fakeProber{})
	empty := fake.NewClientset()
	c.newKube = func(*rest.Config) (kubernetes.Interface, error) { return empty, nil }

	info, err := c.Connect(context.Background(), "alpha")
	if err == nil {
		t.Fatalf("Connect() succeeded with no kube-system namespace, returning UID %q", info.UID)
	}
	if !strings.Contains(err.Error(), "kube-system") {
		t.Errorf("error = %v, want it to name what it could not read", err)
	}
}

// Two clusters at the same address must not share a run directory. This is
// the rebuilt-kind-cluster case, and it is not exotic for a demo tool.
func TestRunDirectoryDiffersForADifferentUIDAtTheSameServer(t *testing.T) {
	root := t.TempDir()
	a := runDir(root, "11111111-2222-3333-4444-555555555555")
	b := runDir(root, "99999999-8888-7777-6666-555555555555")
	if a == b {
		t.Fatalf("two cluster UIDs mapped to the same run directory: %s", a)
	}
}

// The hook is where everything that must be true before the first
// cluster-touching request lands -- the engine's identity today, the
// per-cluster store and recovery later. A connect that returned with the hook
// unfinished would open the gate over half-built wiring.
func TestConnectRunsTheHookBeforeReportingConnected(t *testing.T) {
	c := newTestConnector(t, fakeProber{})

	var sawUID string
	var stateDuringHook connState
	c.onConnected = func(_ context.Context, info ClusterInfo) error {
		sawUID = info.UID
		stateDuringHook = c.State()
		return nil
	}

	if _, err := c.Connect(context.Background(), "alpha"); err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	if sawUID != testClusterUID {
		t.Errorf("the hook saw UID %q, want the cluster's identity", sawUID)
	}
	if stateDuringHook == stateConnected {
		t.Error("the connector reported connected while the hook was still running -- the gate would open over half-built wiring")
	}
}

// A hook that fails fails the connect: whatever it could not establish is
// something a cluster-touching request would have relied on.
func TestConnectFailsWhenTheHookFails(t *testing.T) {
	c := newTestConnector(t, fakeProber{})
	c.onConnected = func(context.Context, ClusterInfo) error {
		return errHookFailed
	}

	if _, err := c.Connect(context.Background(), "alpha"); !errors.Is(err, errHookFailed) {
		t.Fatalf("Connect() error = %v, want the hook's error", err)
	}
	if c.State() != stateDisconnected {
		t.Errorf("State() = %v after a failed hook, want stateDisconnected", c.State())
	}
	if _, _, ok := c.Cluster(); ok {
		t.Error("Cluster() reports a connection whose hook failed")
	}
}

var errHookFailed = errors.New("recovery failed")

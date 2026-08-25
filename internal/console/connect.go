package console

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"time"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// connectTimeout bounds the one cluster round-trip Connect makes. Short
// enough that a wrong context or a sleeping VPN reports quickly, long enough
// for an exec credential plugin to run -- gke-gcloud-auth-plugin and
// `aws eks get-token` both shell out and can take seconds on a cold cache.
const connectTimeout = 30 * time.Second

var (
	errAlreadyConnected = aicrerrors.New(aicrerrors.ErrCodeConflict,
		"this console is already connected to a cluster; restart the binary to use a different one")
	errConnectInFlight = aicrerrors.New(aicrerrors.ErrCodeConflict,
		"a connection attempt is already in progress")
)

// ClusterInfo describes the cluster this process connected to. It is what
// POST /api/connect returns, and what the confirm gate shows the operator
// before anything is installed.
type ClusterInfo struct {
	Context   string `json:"context"`
	Server    string `json:"server"`
	Version   string `json:"version"`
	NodeCount int    `json:"nodeCount"`
	UID       string `json:"uid"`
	// Toolchain is what preflight resolved at startup, carried here because
	// the Connect screen is where the operator confirms what this console is
	// about to do and with what. Spec §7 asks the screen to report the
	// resolved versions; §5 asks the run to record them. Preflight runs once,
	// before connect, so one map answers both.
	Toolchain Toolchain `json:"toolchain,omitempty"`
}

// ContextInfo is one row of the Connect screen's list.
type ContextInfo struct {
	Name    string `json:"name"`
	Server  string `json:"server"`
	Current bool   `json:"current"`
}

type connState int

const (
	stateDisconnected connState = iota
	stateConnecting
	stateConnected
)

func (s connState) String() string {
	switch s {
	case stateDisconnected:
		return "disconnected"
	case stateConnecting:
		return "connecting"
	case stateConnected:
		return "connected"
	default:
		return fmt.Sprintf("connState(%d)", int(s))
	}
}

// loadingRules honors KUBECONFIG and an explicit --kubeconfig path, in
// clientcmd's own precedence order. Building them here rather than at each
// call site means the context list and the connection can never disagree
// about which file they are reading.
func loadingRules(kubeconfig string) *clientcmd.ClientConfigLoadingRules {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	return rules
}

// listContexts reads the kubeconfig and nothing else. No cluster is
// contacted: the operator is choosing which one to talk to, and most of the
// contexts in a working kubeconfig are unreachable from wherever they are
// sitting at the time.
func listContexts(kubeconfig string) ([]ContextInfo, error) {
	cfg, err := loadingRules(kubeconfig).Load()
	if err != nil {
		return nil, fmt.Errorf("reading kubeconfig: %w", err)
	}
	out := make([]ContextInfo, 0, len(cfg.Contexts))
	for name, kctx := range cfg.Contexts {
		info := ContextInfo{Name: name, Current: name == cfg.CurrentContext}
		if cluster, ok := cfg.Clusters[kctx.Cluster]; ok {
			info.Server = cluster.Server
		}
		out = append(out, info)
	}
	// Sorted because the map range above is not an order: the same file would
	// otherwise list its contexts differently on two consecutive loads, and
	// the list moves under the operator's cursor between renders.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// prober is the one cluster round-trip Connect makes, narrowed so tests can
// supply it without a fake clientset for a call that only reads two scalars.
type prober interface {
	probe(ctx context.Context, kube kubernetes.Interface) (version string, nodes int, err error)
}

type liveProber struct{}

func (liveProber) probe(ctx context.Context, kube kubernetes.Interface) (string, int, error) {
	v, err := kube.Discovery().ServerVersion()
	if err != nil {
		return "", 0, fmt.Errorf("asking the cluster for its version: %w", err)
	}
	nodes, err := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", 0, fmt.Errorf("listing nodes: %w", err)
	}
	return v.GitVersion, len(nodes.Items), nil
}

// connector owns the one connection this process will ever have.
//
// State is single-assignment: disconnected -> connecting -> connected, and it
// never leaves connected. A second Connect is refused whether the first is
// still running or already finished, because connect mutates process-global
// KUBECONFIG, builds the clientset the observer and every step read, and
// selects the run directory -- three things that a torn interleaving would
// leave pointing at two different clusters.
//
// It is also why in-session cluster switching is prohibited: a reconnect
// would have to re-derive all four cluster consumers and re-key the run
// directory mid-process. Restarting the binary is cheap.
//
// A failed attempt is the exception: it returns to disconnected, because a
// wrong context or a sleeping VPN is the ordinary case and the operator has
// to be able to pick again without restarting.
type connector struct {
	mu         sync.Mutex
	state      connState
	kubeconfig string
	prober     prober
	// toolchain is reported on the connect response. It is resolved before
	// any connection exists -- it describes this machine, not the cluster --
	// so it is set once at construction rather than discovered in dial.
	toolchain Toolchain
	// newKube builds the clientset from the selected context's rest.Config.
	// A field rather than a direct call so a test can hand dial a fake
	// clientset and exercise the identity read for real, instead of stubbing
	// out the one call the run directory is keyed on.
	newKube func(*rest.Config) (kubernetes.Interface, error)
	// onConnected is everything that has to be true before the console
	// answers a single cluster-touching request: today the engine learning
	// which cluster it is connected to, and later the per-cluster store and
	// the recovery that runs against it. It runs while the connection is
	// still stateConnecting, so a failure leaves the connector reusable and
	// the gate shut rather than open over half-built wiring.
	onConnected func(context.Context, ClusterInfo) error

	// Written once, under mu, at the connected transition.
	info ClusterInfo
	rest *rest.Config
	kube kubernetes.Interface
}

func newConnector(kubeconfig string, p prober) *connector {
	return &connector{
		kubeconfig: kubeconfig,
		prober:     p,
		newKube:    func(cfg *rest.Config) (kubernetes.Interface, error) { return kubernetes.NewForConfig(cfg) },
	}
}

func (c *connector) State() connState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// Cluster returns the connection, or false if none has been established.
//
//nolint:unparam // the clientset is the point of this accessor -- it is what the four cluster consumers are re-derived from once the connect path builds them; today only the bool has a caller.
func (c *connector) Cluster() (kubernetes.Interface, ClusterInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.kube, c.info, c.state == stateConnected
}

// Contexts lists the contexts the operator can choose between.
func (c *connector) Contexts() ([]ContextInfo, error) {
	return listContexts(c.kubeconfig)
}

func (c *connector) Connect(ctx context.Context, contextName string) (ClusterInfo, error) {
	c.mu.Lock()
	if c.state != stateDisconnected {
		state := c.state
		c.mu.Unlock()
		if state == stateConnected {
			return ClusterInfo{}, errAlreadyConnected
		}
		return ClusterInfo{}, errConnectInFlight
	}
	c.state = stateConnecting
	c.mu.Unlock()

	info, restCfg, kube, err := c.dial(ctx, contextName)
	if err == nil && c.onConnected != nil {
		err = c.onConnected(ctx, info)
	}
	if err != nil {
		// Back to disconnected, not stuck in connecting: a wrong context or a
		// sleeping VPN is the ordinary case, and the operator has to be able
		// to pick again without restarting the binary.
		c.mu.Lock()
		c.state = stateDisconnected
		c.mu.Unlock()
		return ClusterInfo{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.info, c.rest, c.kube = info, restCfg, kube
	c.state = stateConnected
	return info, nil
}

func (c *connector) dial(ctx context.Context, contextName string) (ClusterInfo, *rest.Config, kubernetes.Interface, error) {
	overrides := &clientcmd.ConfigOverrides{CurrentContext: contextName}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules(c.kubeconfig), overrides)

	restCfg, err := cc.ClientConfig()
	if err != nil {
		return ClusterInfo{}, nil, nil, fmt.Errorf("building a client for context %q: %w", contextName, err)
	}
	kube, err := c.newKube(restCfg)
	if err != nil {
		return ClusterInfo{}, nil, nil, fmt.Errorf("building a clientset for context %q: %w", contextName, err)
	}

	probeCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	version, nodes, err := c.prober.probe(probeCtx, kube)
	if err != nil {
		return ClusterInfo{}, nil, nil, fmt.Errorf("reaching context %q at %s: %w", contextName, restCfg.Host, err)
	}

	// The kube-system UID is this cluster's identity, and it is what keys the
	// run directory. Neither the server URL nor the context name will do: a
	// context is a label in a file the operator edits, and an address can
	// front a rebuilt cluster -- `kind delete && kind create` reuses the
	// endpoint. The UID changes when the cluster does, which is the property
	// the key needs.
	//
	// kube-system is created by the control plane at bootstrap, is never
	// recreated during a cluster's life, and is readable by any principal
	// that can do anything else useful here.
	//
	// A failure here fails the connect rather than degrading to an empty UID:
	// every cluster whose identity could not be read would otherwise share
	// one run directory, which is the collision the UID exists to prevent.
	ns, err := kube.CoreV1().Namespaces().Get(probeCtx, "kube-system", metav1.GetOptions{})
	if err != nil {
		return ClusterInfo{}, nil, nil, fmt.Errorf("reading this cluster's identity from kube-system: %w", err)
	}

	return ClusterInfo{
		Context:   contextName,
		Server:    restCfg.Host,
		Version:   version,
		NodeCount: nodes,
		UID:       string(ns.UID),
		Toolchain: c.toolchain,
	}, restCfg, kube, nil
}

// runDir is the per-cluster run directory. The ConfigMap store got this
// property free by living inside the cluster it described; a flat local file
// does not. An operator who demos cluster A then connects to cluster B would
// otherwise have B's console recover A's run and offer a Reset that
// uninstalls releases in the wrong place.
func runDir(workDir, clusterUID string) string {
	return filepath.Join(workDir, "runs", clusterUID)
}

// clusterGate adapts the connector to the seam internal/api gates its routes
// on. The interface is declared there, in terms of values it only marshals,
// so the dependency stays one-way: console imports api, never the reverse.
type clusterGate struct{ c *connector }

func (g clusterGate) Contexts() (any, error) { return g.c.Contexts() }

func (g clusterGate) Connect(ctx context.Context, contextName string) (any, error) {
	return g.c.Connect(ctx, contextName)
}

func (g clusterGate) Connected() bool {
	_, _, ok := g.c.Cluster()
	return ok
}

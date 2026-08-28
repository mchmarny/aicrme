package console

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/mchmarny/aicrme/internal/engine"
	"github.com/mchmarny/aicrme/internal/steps"
	corev1 "k8s.io/api/core/v1"
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
	// Nodes is what the cluster is made of, folded into shapes, plus a verdict
	// on whether the snapshot agent can reach the GPU ones.
	//
	// Here rather than on a later screen because the answer is only actionable
	// here: the tolerations are read from the process environment at startup
	// and Connect is single-assignment, so a GPU pool the agent cannot reach is
	// fixed by quitting and relaunching. That is cheap before anything is
	// installed and expensive after.
	Nodes NodeComposition `json:"nodes"`
	UID   string          `json:"uid"`
	// Toolchain is what preflight resolved at startup, carried here because
	// the Connect screen is where the operator confirms what this console is
	// about to do and with what. Spec §7 asks the screen to report the
	// resolved versions; §5 asks the run to record them. Preflight runs once,
	// before connect, so one map answers both.
	Toolchain Toolchain `json:"toolchain,omitempty"`
	// RegistryWarning names a helm credential helper this machine does not
	// have, when helm's registry config names one. Empty on a healthy machine,
	// which is the ordinary case. See checkHelmCredentialHelpers for why this
	// is worth saying before an install rather than during one.
	RegistryWarning string `json:"registryWarning,omitempty"`
	// RecoveredRun is the run this cluster's store was holding, if any.
	//
	// It travels in the connect response because connect is now the only
	// moment recovery can happen: the store lives under a directory named for
	// a cluster this process had not chosen until this call. In-cluster the
	// pod restart was the trigger and the SPA simply found a run waiting on
	// its first GET; locally there is no restart, so the response has to say
	// so or the SPA lands in an empty state over a record that still describes
	// releases on the cluster.
	RecoveredRun *engine.Run `json:"recoveredRun,omitempty"`
	// GPUTolerations is what the agent Job and the Prove workload will carry:
	// AICRME_GPU_TOLERATIONS, plus whatever Connect derived from this
	// cluster's GPU pool. Not serialized -- the browser is shown
	// Nodes.Tolerating, which is the same information in the spelling an
	// operator reads. This field exists so connectHook wires the run with the
	// set the verdict above was computed against.
	GPUTolerations []corev1.Toleration `json:"-"`
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

// nodePageSize bounds each page of the node List.
//
// One unbounded List of a large cluster is a single very large response --
// Node objects are fat, status.images alone runs to tens of kilobytes each --
// and it is the shape most likely to time out or be refused outright. Paging
// costs an extra round trip on clusters big enough to need one and nothing at
// all on clusters that are not.
const nodePageSize = 500

// prober is the one cluster round-trip Connect makes.
//
// It returns the nodes rather than a verdict about them: this is the I/O seam,
// and which taints matter is policy that belongs with the tolerations the run
// will actually use. Keeping the split means the fold and the verdict are
// testable without a cluster at all.
type prober interface {
	probe(ctx context.Context, kube kubernetes.Interface) (version string, nodes []corev1.Node, err error)
}

type liveProber struct{}

func (liveProber) probe(ctx context.Context, kube kubernetes.Interface) (string, []corev1.Node, error) {
	v, err := kube.Discovery().ServerVersion()
	if err != nil {
		return "", nil, fmt.Errorf("asking the cluster for its version: %w", err)
	}

	var out []corev1.Node
	// ResourceVersion "0" is served from the apiserver's watch cache rather
	// than etcd. This is a display read taken once at connect; a slightly stale
	// node list is not a correctness problem, and the cheaper path is the right
	// default for the screen an operator sees before anything is installed.
	opts := metav1.ListOptions{Limit: nodePageSize, ResourceVersion: "0"}
	for {
		page, err := kube.CoreV1().Nodes().List(ctx, opts)
		if err != nil {
			return "", nil, fmt.Errorf("listing nodes: %w", err)
		}
		out = append(out, page.Items...)
		if page.Continue == "" {
			break
		}
		// A continue token is only valid against the exact snapshot it came
		// from, so the resourceVersion hint has to be dropped on later pages.
		opts.Continue = page.Continue
		opts.ResourceVersion = ""
	}
	return v.GitVersion, out, nil
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
	// registryWarning is checkHelmCredentialHelpers' finding, resolved at
	// startup alongside the toolchain and for the same reason: it describes
	// this machine, not the cluster. It rides on the connect response so the
	// operator reads it on the screen where they are already deciding whether
	// to proceed, rather than in a terminal they may not be watching.
	registryWarning string
	// preferred is --context: the one the Connect screen arrives preselected
	// on. Empty leaves the kubeconfig's own current-context marked. See
	// Contexts for why this preselects rather than connects.
	preferred string
	// gpuTolerations is AICRME_GPU_TOLERATIONS, the same value handed to the
	// snapshot agent and the Prove workload. It is here so the connect response
	// can report whether the GPU nodes it found are reachable BY THE RUN THAT
	// WILL ACTUALLY HAPPEN, rather than by some default the run does not use.
	gpuTolerations []corev1.Toleration
	// newKube builds the clientset from the selected context's rest.Config.
	// A field rather than a direct call so a test can hand dial a fake
	// clientset and exercise the identity read for real, instead of stubbing
	// out the one call the run directory is keyed on.
	newKube func(*rest.Config) (kubernetes.Interface, error)
	// onConnected is everything that has to be true before the console
	// answers a single cluster-touching request: the frozen kubeconfig, the
	// per-cluster store, the steps and the two cluster clients the engine
	// runs on, and the recovery that runs against them. It runs while the
	// connection is still stateConnecting, so a failure leaves the connector
	// reusable and the gate shut rather than open over half-built wiring.
	//
	// It takes *ClusterInfo rather than a copy because recovery is part of
	// what it does and the recovered run is reported in the same response.
	onConnected func(context.Context, *ClusterInfo, kubernetes.Interface) error

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

// Contexts lists the contexts the operator can choose between, with --context
// (if given) marked current in place of the kubeconfig's own choice.
//
// Marked, not selected: --context says which one the Connect screen should
// arrive preselected on, and the operator still confirms it. A flag that
// connected on their behalf would make the one irreversible decision this
// console has -- which cluster -- without anyone looking at it.
//
// A name that is not in the kubeconfig leaves the list exactly as it was. It
// is reported at the point it becomes actionable: the screen shows the real
// contexts and the operator picks, rather than the binary refusing to start
// over a typo in a preselection.
func (c *connector) Contexts() ([]ContextInfo, error) {
	out, err := listContexts(c.kubeconfig)
	if err != nil || c.preferred == "" {
		return out, err
	}
	var found bool
	for i := range out {
		if out[i].Name == c.preferred {
			found = true
		}
	}
	if !found {
		slog.Warn("the requested context is not in this kubeconfig; leaving the kubeconfig's own current-context selected",
			"context", c.preferred)
		return out, nil
	}
	for i := range out {
		out[i].Current = out[i].Name == c.preferred
	}
	return out, nil
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
		err = c.onConnected(ctx, &info, kube)
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
	// The recovered run is reported to the caller of THIS connect and kept out
	// of the stored connection: it is a fact about one instant, and the run it
	// describes moves on. A later reader -- the reload path behind GET
	// /api/cluster -- gets the live run from the run routes instead of a
	// snapshot that was true once.
	c.info.RecoveredRun = nil
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

	// Derive the GPU pool's own taints and adopt them, rather than reporting
	// them and asking the operator to relaunch. This is the whole of what
	// AICRME_GPU_TOLERATIONS did on a real cluster, and it was never a
	// discovery problem: this function already had the nodes in hand and the
	// screen already printed the exact string. What it could not do was reach
	// the process's own startup configuration, which is where the value lived.
	//
	// Computed against steps.AgentTolerations rather than against
	// c.gpuTolerations alone, because what decides whether the agent Job can
	// land is the whole set it will carry -- the built-in nvidia.com/gpu
	// toleration included. Deriving a pool the built-in already covers would
	// add a redundant toleration on the ordinary cluster.
	var derived []string
	effective := c.gpuTolerations
	for _, t := range untoleratedGPUPoolTaints(nodes, steps.AgentTolerations(c.gpuTolerations)) {
		effective = append(effective, tolerationFor(t))
		derived = append(derived, formatTaint(t))
	}

	composition := groupNodes(nodes, steps.AgentTolerations(effective))
	composition.Tolerating = strings.Join(derived, ",")

	return ClusterInfo{
		Context:   contextName,
		Server:    restCfg.Host,
		Version:   version,
		NodeCount: len(nodes),
		Nodes:     composition,
		UID:       string(ns.UID),
		Toolchain: c.toolchain,
		// Carried on every connect response, not just the first: the Connect
		// screen is re-rendered per attempt and the condition does not change
		// between them.
		RegistryWarning: c.registryWarning,
		// The set the run will really carry, handed to the wiring rather than
		// re-derived there: the agent Job and the Prove workload must tolerate
		// the same taints as the pool this screen just judged reachable, and
		// two derivation sites is how those drift apart.
		GPUTolerations: effective,
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

func (g clusterGate) Info() (any, bool) {
	_, info, ok := g.c.Cluster()
	if !ok {
		return nil, false
	}
	return info, true
}

package console

import (
	"sort"
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
)

// gpuResource is the extended resource a GPU node advertises once its device
// plugin is up. Nodes are identified as GPU nodes by capacity rather than by
// label because labels are per-platform -- GKE writes
// cloud.google.com/gke-accelerator, EKS and AKS write something else -- while
// this resource name is what every one of them ends up reporting.
const gpuResource = "nvidia.com/gpu"

// instanceTypeLabel and acceleratorLabel are descriptive only. They title the
// group for a human; nothing keys a decision on them.
const (
	instanceTypeLabel = "node.kubernetes.io/instance-type"
	acceleratorLabel  = "cloud.google.com/gke-accelerator"
)

// kwokNodeTaint marks a KWOK fake node.
//
// Such a node is unreachable by the agent on purpose: steps.AgentTolerations
// refuses a blanket toleration so the agent cannot land somewhere KWOK reports
// Succeeded without executing anything. Calling that "blocked" would be true
// and harmful -- the remedy it implies is the one toleration that turns a
// working demo into a silent false success -- so these groups are described
// and excluded from the verdict.
const kwokNodeTaint = "kwok.x-k8s.io/node"

// maxNodeGroups bounds the rows this screen renders. Folding already collapses
// a homogeneous cluster to a handful; this covers the heterogeneous one, where
// the shape count is genuinely large and a full list would stop being a
// summary. The remainder is counted in NodeComposition.More, never dropped
// silently.
const maxNodeGroups = 8

// transientTaints are taints that describe a node's momentary condition rather
// than the shape of the pool it belongs to.
//
// Two reasons to drop them, and the second is the load-bearing one:
//
// They are noise in the verdict -- a node that is NotReady or draining is not
// a misconfigured GPU pool, and telling an operator to tolerate
// node.kubernetes.io/unreachable would be absurd advice.
//
// And cluster-autoscaler writes ToBeDeletedByClusterAutoscaler with a unix
// TIMESTAMP as the value, unique per node. Folding on a key that includes it
// turns a 300-node cluster mid-scaledown into 300 groups, which defeats the
// only thing that keeps this screen independent of cluster size.
//
// The node.kubernetes.io/* set comes from the API package's own constants
// rather than string literals, so a rename upstream is a compile error here
// instead of a silently dead filter.
var transientTaints = map[string]bool{
	corev1.TaintNodeNotReady:           true,
	corev1.TaintNodeUnreachable:        true,
	corev1.TaintNodeUnschedulable:      true,
	corev1.TaintNodeMemoryPressure:     true,
	corev1.TaintNodeDiskPressure:       true,
	corev1.TaintNodeNetworkUnavailable: true,
	corev1.TaintNodePIDPressure:        true,
	corev1.TaintNodeOutOfService:       true,
	// Not in the API package: cluster-autoscaler is a separate project and
	// these are its own, spelled as it writes them.
	"ToBeDeletedByClusterAutoscaler":       true,
	"DeletionCandidateOfClusterAutoscaler": true,
}

// NodeGroup folds nodes that share a shape and a scheduling constraint.
type NodeGroup struct {
	Count        int      `json:"count"`
	InstanceType string   `json:"instanceType,omitempty"`
	Accelerator  string   `json:"accelerator,omitempty"`
	GPUsPerNode  int64    `json:"gpusPerNode,omitempty"`
	Taints       []string `json:"taints,omitempty"`
	// Blocked marks a GPU group the snapshot agent cannot schedule onto: it
	// carries a taint no toleration in the run's set matches. Only ever set on
	// a group with GPUs -- a tainted CPU node is not a problem to report,
	// because nothing wants to land there.
	Blocked bool `json:"blocked,omitempty"`
	// Simulated marks a KWOK fake node. Such a node is unreachable by design
	// and must never be reported Blocked; see kwokNodeTaint.
	Simulated bool `json:"simulated,omitempty"`
}

// NodeComposition is what the Connect screen renders.
//
// Folded server-side, deliberately. A cluster can have hundreds of nodes and
// each Node object is fat, so sending per-node data to a browser would make
// this screen scale with the cluster. Groups do not.
type NodeComposition struct {
	Total    int         `json:"total"`
	GPUNodes int         `json:"gpuNodes"`
	Groups   []NodeGroup `json:"groups,omitempty"`
	More     int         `json:"more,omitempty"`
	// TotalGPUs and UsableGPUs are cluster-wide, summed across every node.
	//
	// They exist because the snapshot cannot answer this. gap.Analyze derives
	// its headline from the GPU hardware subtype, which is an in-pod PCI probe
	// describing the single node the agent landed on -- AICR's own log warns
	// that "the GPU collector samples a single node". On a two-node H100
	// cluster that reported "8 of 8" for sixteen GPUs.
	//
	// Total is capacity and Usable is allocatable, because they answer
	// different questions: what the hardware has, versus what a workload could
	// schedule onto right now. Both are zero on a cluster whose device plugin
	// has not come up, since nvidia.com/gpu is published by that plugin -- and
	// zero is the signal gap.Analyze needs to fall back to the probe rather
	// than report a confident nothing.
	TotalGPUs  int64 `json:"totalGPUs,omitempty"`
	UsableGPUs int64 `json:"usableGPUs,omitempty"`
	// Remedy is the AICRME_GPU_TOLERATIONS value that would clear every
	// blocked group, in the exact spelling parseTolerations reads back.
	//
	// Now a genuine last resort rather than the normal path: Connect derives
	// the GPU pool's own taints and tolerates them, so a group is Blocked only
	// if it is unreachable for a reason derivation deliberately will not fix --
	// today that means a simulated pool, which is excluded from the verdict
	// anyway. Kept because "the agent cannot reach your GPUs and here is the
	// exact string" is still the right thing to say if that ever changes.
	Remedy string `json:"remedy,omitempty"`
	// Tolerating is what Connect derived from this cluster's GPU nodes and
	// added to the run, same spelling as Remedy. Empty when the built-in
	// nvidia.com/gpu toleration already covered everything.
	//
	// Rendered rather than kept internal on purpose. Tolerating a taint is a
	// scheduling decision made on the operator's behalf, and a taint can exist
	// precisely to keep other people's workloads off a pool -- so the screen
	// has to say which ones this run will ignore, before it installs anything.
	Tolerating string `json:"tolerating,omitempty"`
}

// groupNodes folds a node list into shapes and reports which GPU shapes the
// agent cannot reach.
//
// tolerations must be the set the agent Job will really carry
// (steps.AgentTolerations), not a reconstruction of it.
func groupNodes(nodes []corev1.Node, tolerations []corev1.Toleration) NodeComposition {
	comp := NodeComposition{Total: len(nodes)}
	index := map[string]int{}
	var remedy []string
	seen := map[string]bool{}

	for i := range nodes {
		g := describe(&nodes[i], tolerations)
		if g.GPUsPerNode > 0 {
			comp.GPUNodes++
			comp.TotalGPUs += gpuQuantity(nodes[i].Status.Capacity)
			comp.UsableGPUs += gpuQuantity(nodes[i].Status.Allocatable)
		}
		if g.Blocked {
			for _, t := range untoleratedTaints(&nodes[i], tolerations) {
				if s := formatTaint(t); !seen[s] {
					seen[s] = true
					remedy = append(remedy, s)
				}
			}
		}
		key := groupKey(g)
		if at, ok := index[key]; ok {
			comp.Groups[at].Count++
			continue
		}
		index[key] = len(comp.Groups)
		comp.Groups = append(comp.Groups, g)
	}

	// Blocked first, then by size. Ordering by size alone would sort the one
	// group that needs reading to the bottom and then let the cap delete it: a
	// GPU pool is routinely the smallest pool in the cluster.
	sort.SliceStable(comp.Groups, func(i, j int) bool {
		if comp.Groups[i].Blocked != comp.Groups[j].Blocked {
			return comp.Groups[i].Blocked
		}
		return comp.Groups[i].Count > comp.Groups[j].Count
	})
	if len(comp.Groups) > maxNodeGroups {
		comp.More = len(comp.Groups) - maxNodeGroups
		comp.Groups = comp.Groups[:maxNodeGroups]
	}

	comp.Remedy = strings.Join(remedy, ",")
	return comp
}

// describe reduces one node to its group shape.
func describe(n *corev1.Node, tolerations []corev1.Toleration) NodeGroup {
	g := NodeGroup{
		Count:        1,
		InstanceType: n.Labels[instanceTypeLabel],
		Accelerator:  n.Labels[acceleratorLabel],
		GPUsPerNode:  gpuCapacity(n),
	}
	for _, t := range n.Spec.Taints {
		if transientTaints[t.Key] {
			continue
		}
		g.Taints = append(g.Taints, formatTaint(t))
		if t.Key == kwokNodeTaint {
			g.Simulated = true
		}
	}
	g.Blocked = g.GPUsPerNode > 0 && !g.Simulated && len(untoleratedTaints(n, tolerations)) > 0
	return g
}

// untoleratedGPUPoolTaints returns the distinct taints carried by this
// cluster's REAL GPU nodes that tolerations does not already match.
//
// This is what makes the GPU taint stop being a flag. The console already
// computed exactly this set in order to print
// `AICRME_GPU_TOLERATIONS=<taint>` on the Connect screen and ask the operator
// to quit and relaunch; the value was never unknown, it was only unreachable
// from the running process. Deriving it here and wiring it into the run is
// the same answer applied instead of displayed.
//
// TWO EXCLUSIONS ARE LOAD-BEARING, and both are the reason this is a
// narrow derivation rather than `{Operator: Exists}`:
//
// Simulated nodes are skipped entirely. KWOK fakes Running/Succeeded for
// anything scheduled onto a fake node without executing it, so an agent that
// tolerates kwok.x-k8s.io/node reports a successful snapshot having collected
// nothing -- see steps.AgentTolerations, which refuses a blanket toleration
// for this reason. Deriving from whatever the nodes carry would have
// reintroduced that automatically, and on the console's own e2e cluster.
//
// Non-GPU nodes are skipped. A taint on a CPU node is somebody else's
// reservation, and neither consumer of this set has any business there: the
// agent must land on a GPU node to probe an accelerator, and the Prove
// workload requests GPUs.
//
// Transient taints are dropped by the same filter the verdict uses -- a
// draining node is not a misconfigured pool.
func untoleratedGPUPoolTaints(nodes []corev1.Node, tolerations []corev1.Toleration) []corev1.Taint {
	var out []corev1.Taint
	seen := map[string]bool{}
	for i := range nodes {
		g := describe(&nodes[i], tolerations)
		if g.GPUsPerNode == 0 || g.Simulated {
			continue
		}
		for _, t := range untoleratedTaints(&nodes[i], tolerations) {
			if key := formatTaint(t); !seen[key] {
				seen[key] = true
				out = append(out, t)
			}
		}
	}
	return out
}

// tolerationFor is the narrowest toleration that matches one taint.
//
// Equal on the key AND the value AND the effect, never Exists: a derived
// toleration is one the operator did not ask for, so it must buy exactly the
// node it was derived from and nothing else. Exists on a key would tolerate
// every value of it -- a `dedicated=gpu-workload` derivation would silently
// also accept `dedicated=someone-elses-reservation`.
func tolerationFor(t corev1.Taint) corev1.Toleration {
	return corev1.Toleration{
		Key:      t.Key,
		Operator: corev1.TolerationOpEqual,
		Value:    t.Value,
		Effect:   t.Effect,
	}
}

// untoleratedTaints returns the taints on n that nothing in tolerations
// matches.
func untoleratedTaints(n *corev1.Node, tolerations []corev1.Toleration) []corev1.Taint {
	var out []corev1.Taint
	for _, t := range n.Spec.Taints {
		if transientTaints[t.Key] {
			continue
		}
		if !tolerated(t, tolerations) {
			out = append(out, t)
		}
	}
	return out
}

// tolerated reports whether any toleration matches the taint, using the API
// package's own matcher rather than a local reimplementation of scheduler
// semantics.
//
// The logger is discarded and comparison operators are disabled because both
// are only reachable through the Lt and Gt operators, and parseTolerations
// constructs nothing but Exists and Equal.
func tolerated(t corev1.Taint, tolerations []corev1.Toleration) bool {
	for i := range tolerations {
		if tolerations[i].ToleratesTaint(logr.Discard(), &t, false) {
			return true
		}
	}
	return false
}

// gpuCapacity reads the node's advertised GPU count, preferring allocatable.
func gpuCapacity(n *corev1.Node) int64 {
	if v := gpuQuantity(n.Status.Allocatable); v > 0 {
		return v
	}
	return gpuQuantity(n.Status.Capacity)
}

// gpuQuantity reads nvidia.com/gpu out of one resource list.
func gpuQuantity(rl corev1.ResourceList) int64 {
	if q, ok := rl[gpuResource]; ok {
		return q.Value()
	}
	return 0
}

// formatTaint spells a taint the way AICRME_GPU_TOLERATIONS is read back, so
// what this screen prints can be pasted without translation.
func formatTaint(t corev1.Taint) string {
	var b strings.Builder
	b.WriteString(t.Key)
	if t.Value != "" {
		b.WriteString("=")
		b.WriteString(t.Value)
	}
	if t.Effect != "" {
		b.WriteString(":")
		b.WriteString(string(t.Effect))
	}
	return b.String()
}

func groupKey(g NodeGroup) string {
	return strings.Join([]string{
		g.InstanceType,
		g.Accelerator,
		strings.Join(g.Taints, ","),
	}, "\x00")
}

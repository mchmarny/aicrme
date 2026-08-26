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
	// Remedy is the AICRME_GPU_TOLERATIONS value that would clear every
	// blocked group, in the exact spelling parseTolerations reads back.
	Remedy string `json:"remedy,omitempty"`
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
	if q, ok := n.Status.Allocatable[gpuResource]; ok {
		return q.Value()
	}
	if q, ok := n.Status.Capacity[gpuResource]; ok {
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

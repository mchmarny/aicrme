package console

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/mchmarny/aicrme/internal/steps"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// taint is shorthand for the NoSchedule taints these tests care about.
func taint(key, value string) corev1.Taint {
	return corev1.Taint{Key: key, Value: value, Effect: corev1.TaintEffectNoSchedule}
}

// gpusPerTestNode is what an a3-megagpu-8g reports, and what every node these
// tests build advertises. A per-call parameter would suggest the count changes
// a decision somewhere; it does not -- it is carried for display only.
const gpusPerTestNode = 8

// gpuNode reports allocatable GPUs, the way a real GPU pool member does once
// the device plugin is up. The GKE accelerator label is included because it is
// what the first real cluster this console met actually carried.
func gpuNode(name, instanceType, accelerator string, taints ...corev1.Taint) corev1.Node {
	n := cpuNode(name, instanceType, taints...)
	n.Labels["cloud.google.com/gke-accelerator"] = accelerator
	n.Status.Allocatable = corev1.ResourceList{
		"nvidia.com/gpu": *resource.NewQuantity(gpusPerTestNode, resource.DecimalSI),
	}
	return n
}

func cpuNode(name, instanceType string, taints ...corev1.Taint) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{"node.kubernetes.io/instance-type": instanceType},
		},
		Spec: corev1.NodeSpec{Taints: taints},
	}
}

// TestGroupNodesFlagsUntoleratedGPUTaint is the Phase 4 failure, and the one
// this whole feature exists for: the GPU pool carries a taint of the platform
// team's own choosing, the built-in nvidia.com/gpu toleration does not match
// it, and nothing says so until Discover has sat Pending for ten minutes.
func TestGroupNodesFlagsUntoleratedGPUTaint(t *testing.T) {
	nodes := []corev1.Node{
		gpuNode("gpu-a", "a3-megagpu-8g", "nvidia-h100-mega-80gb", taint("dedicated", "gpu-workload")),
	}

	got := groupNodes(nodes, steps.AgentTolerations(nil))

	if len(got.Groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(got.Groups))
	}
	if !got.Groups[0].Blocked {
		t.Error("a GPU node tainted dedicated=gpu-workload:NoSchedule is not reachable by the built-in tolerations, so its group should be Blocked")
	}
	if want := "dedicated=gpu-workload:NoSchedule"; got.Remedy != want {
		t.Errorf("Remedy = %q, want %q -- the value an operator pastes into AICRME_GPU_TOLERATIONS", got.Remedy, want)
	}
}

// TestGroupNodesDoesNotFlagSimulatedGPUNodes guards the demo path.
//
// KWOK's fake GPU nodes carry kwok.x-k8s.io/node=fake:NoSchedule, and the
// agent deliberately does not tolerate it -- steps.AgentTolerations refuses a
// blanket toleration precisely so the agent cannot land on a node KWOK would
// report Succeeded for without running anything. So these nodes really are
// unreachable, on purpose.
//
// Reporting that as a problem would be true and useless: it would prompt the
// operator to set AICRME_GPU_TOLERATIONS to the very taint whose whole purpose
// is to stay untolerated, turning a working demo into a silent false success.
// Every `make demo` run would show the warning, which is how a warning stops
// being read.
func TestGroupNodesDoesNotFlagSimulatedGPUNodes(t *testing.T) {
	nodes := []corev1.Node{
		gpuNode("kwok-0", "kwok", "", taint("kwok.x-k8s.io/node", "fake")),
		gpuNode("kwok-1", "kwok", "", taint("kwok.x-k8s.io/node", "fake")),
	}

	got := groupNodes(nodes, steps.AgentTolerations(nil))

	if len(got.Groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(got.Groups))
	}
	if got.Groups[0].Blocked {
		t.Error("simulated KWOK nodes are unreachable by design, so the group must not be reported Blocked")
	}
	if !got.Groups[0].Simulated {
		t.Error("a node carrying the KWOK fake-node taint should be marked Simulated, so the screen can say what it is")
	}
	if got.Remedy != "" {
		t.Errorf("Remedy = %q, want empty -- advising a toleration for the KWOK taint is advising a false success", got.Remedy)
	}
}

// TestGroupNodesFoldsNodesDrainingUnderTheAutoscaler is the scale case.
//
// cluster-autoscaler stamps ToBeDeletedByClusterAutoscaler with a unix
// timestamp as the VALUE, so every draining node carries a taint unique to
// itself. Key the fold on that and a 300-node cluster mid-scaledown produces
// 300 groups -- the screen becomes a node list, which is the thing folding
// exists to prevent. The taint is also transient and says nothing durable
// about whether the pool is reachable, so it is excluded from the verdict too.
func TestGroupNodesFoldsNodesDrainingUnderTheAutoscaler(t *testing.T) {
	nodes := make([]corev1.Node, 0, 300)
	for i := range 300 {
		nodes = append(nodes, gpuNode(
			fmt.Sprintf("gpu-%d", i), "a3-megagpu-8g", "nvidia-h100-mega-80gb",
			taint("ToBeDeletedByClusterAutoscaler", strconv.Itoa(1700000000+i)),
		))
	}

	got := groupNodes(nodes, steps.AgentTolerations(nil))

	if len(got.Groups) != 1 {
		t.Fatalf("groups = %d, want 1 -- a per-node taint value must not become a per-node group", len(got.Groups))
	}
	if got.Groups[0].Count != 300 {
		t.Errorf("Count = %d, want 300", got.Groups[0].Count)
	}
	if got.Groups[0].Blocked {
		t.Error("a node draining under the autoscaler is transiently unschedulable, not a misconfigured pool; it must not be reported Blocked")
	}
	for _, s := range got.Groups[0].Taints {
		if strings.HasPrefix(s, "ToBeDeletedByClusterAutoscaler") {
			t.Errorf("Taints contains %q; a transient taint is not part of the shape", s)
		}
	}
}

// TestGroupNodesCapsShapesAndReportsTheRemainder keeps the screen bounded even
// when folding cannot bound it. A cluster with many small heterogeneous pools
// is legitimate, and a list that grows with it is not a summary.
//
// The kept groups are the largest, and the tail is counted rather than
// silently dropped -- a screen that quietly omits a shape is a screen that
// says "this is your cluster" and is wrong.
func TestGroupNodesCapsShapesAndReportsTheRemainder(t *testing.T) {
	var nodes []corev1.Node
	for shape := range 12 {
		// Shape i gets i+1 nodes, so the largest shapes are unambiguous.
		for n := range shape + 1 {
			nodes = append(nodes, cpuNode(fmt.Sprintf("n-%d-%d", shape, n), fmt.Sprintf("type-%d", shape)))
		}
	}

	got := groupNodes(nodes, steps.AgentTolerations(nil))

	if len(got.Groups) != maxNodeGroups {
		t.Fatalf("groups = %d, want %d", len(got.Groups), maxNodeGroups)
	}
	if want := 12 - maxNodeGroups; got.More != want {
		t.Errorf("More = %d, want %d -- the shapes not shown must still be counted", got.More, want)
	}
	if got.Groups[0].InstanceType != "type-11" {
		t.Errorf("first group = %q, want type-11 -- groups are ordered by size so the cap keeps the largest", got.Groups[0].InstanceType)
	}
	if got.Total != len(nodes) {
		t.Errorf("Total = %d, want %d -- capping the display must not change the node count", got.Total, len(nodes))
	}
}

// TestGroupNodesKeepsBlockedGroupsThroughTheCap covers the collision between
// the two rules above: order by size, and show at most maxNodeGroups.
//
// A GPU pool is routinely the SMALLEST pool in a cluster -- two H100 nodes
// beside forty CPU nodes is the ordinary shape. Rank purely by count on a
// heterogeneous cluster and the one group the operator has to see is the first
// one the cap discards, leaving a screen that looks healthy while Discover is
// about to fail.
func TestGroupNodesKeepsBlockedGroupsThroughTheCap(t *testing.T) {
	nodes := make([]corev1.Node, 0, 10*(maxNodeGroups+3)+1)
	for shape := range maxNodeGroups + 3 {
		for n := range 10 {
			nodes = append(nodes, cpuNode(fmt.Sprintf("cpu-%d-%d", shape, n), fmt.Sprintf("type-%d", shape)))
		}
	}
	// One GPU node, blocked, and the smallest group in the cluster.
	nodes = append(nodes, gpuNode("gpu-lonely", "a3-megagpu-8g", "nvidia-h100-mega-80gb",
		taint("dedicated", "gpu-workload")))

	got := groupNodes(nodes, steps.AgentTolerations(nil))

	var found bool
	for _, g := range got.Groups {
		if g.Blocked {
			found = true
		}
	}
	if !found {
		t.Error("the blocked GPU group was dropped by the cap; it is the one group that must always survive")
	}
	if got.Remedy == "" {
		t.Error("Remedy is empty -- a blocked group that survives must still carry its remedy")
	}
}

// TestRemedyRoundTripsThroughParseTolerations is the property the whole screen
// rests on: the string it tells the operator to paste has to be a string this
// binary reads back, and reading it back has to actually clear the block.
//
// The two halves are written by different code -- formatTaint spells it,
// parseTolerations parses it -- so nothing but a test holds them together. A
// remedy that renders beautifully and parses to something that does not match
// is worse than no remedy, because the operator retries with it and gets the
// same ten-minute timeout.
func TestRemedyRoundTripsThroughParseTolerations(t *testing.T) {
	nodes := []corev1.Node{
		gpuNode("gpu-a", "a3-megagpu-8g", "nvidia-h100-mega-80gb",
			taint("dedicated", "gpu-workload")),
		gpuNode("gpu-b", "a3-megagpu-8g", "nvidia-h100-mega-80gb",
			corev1.Taint{Key: "reserved", Effect: corev1.TaintEffectNoExecute}),
	}

	first := groupNodes(nodes, steps.AgentTolerations(nil))
	if first.Remedy == "" {
		t.Fatal("no remedy produced for two distinctly tainted GPU shapes")
	}

	// Feed the remedy back the way an operator would: into the environment
	// variable, through the same parser the console uses at startup.
	second := groupNodes(nodes, steps.AgentTolerations(parseTolerations(first.Remedy)))

	for _, g := range second.Groups {
		if g.Blocked {
			t.Errorf("group %+v is still Blocked after applying Remedy %q", g, first.Remedy)
		}
	}
	if second.Remedy != "" {
		t.Errorf("Remedy = %q after applying the previous remedy, want empty", second.Remedy)
	}
}

// TestGroupNodesLeavesStandardGPUTaintAlone is the no-noise guard.
//
// nvidia.com/gpu=present:NoSchedule is what GKE's own documentation
// prescribes, and steps.AgentTolerations already covers it. If this warned,
// it would warn on the common case, and a warning shown on every healthy
// cluster is one nobody reads on the unhealthy one.
func TestGroupNodesLeavesStandardGPUTaintAlone(t *testing.T) {
	nodes := []corev1.Node{
		gpuNode("gpu-a", "a3-highgpu-8g", "nvidia-h100-80gb", taint("nvidia.com/gpu", "present")),
	}

	got := groupNodes(nodes, steps.AgentTolerations(nil))

	if got.Groups[0].Blocked {
		t.Error("nvidia.com/gpu=present:NoSchedule is covered by the built-in toleration and must not be reported Blocked")
	}
	if got.Remedy != "" {
		t.Errorf("Remedy = %q, want empty on a conventionally tainted GPU pool", got.Remedy)
	}
	if got.GPUNodes != 1 {
		t.Errorf("GPUNodes = %d, want 1", got.GPUNodes)
	}
}

// TestConnectReportsUnreachableGPUNodes runs the real probe against a fake
// cluster, so the List, the fold and the verdict are exercised together rather
// than each in isolation.
//
// It connects with the tolerations an operator who set nothing would have,
// against the node layout of the GKE cluster this feature was written for.
func TestConnectReportsUnreachableGPUNodes(t *testing.T) {
	gpuA := gpuNode("gpu-a", "a3-megagpu-8g", "nvidia-h100-mega-80gb", taint("dedicated", "gpu-workload"))
	gpuB := gpuNode("gpu-b", "a3-megagpu-8g", "nvidia-h100-mega-80gb", taint("dedicated", "gpu-workload"))
	cpu := cpuNode("cpu-a", "e2-standard-4")

	c := newTestConnector(t, liveProber{}, kubeSystem(testClusterUID), &gpuA, &gpuB, &cpu)

	info, err := c.Connect(t.Context(), "alpha")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	if info.NodeCount != 3 {
		t.Errorf("NodeCount = %d, want 3", info.NodeCount)
	}
	if info.Nodes.GPUNodes != 2 {
		t.Errorf("Nodes.GPUNodes = %d, want 2", info.Nodes.GPUNodes)
	}
	if want := "dedicated=gpu-workload:NoSchedule"; info.Nodes.Remedy != want {
		t.Errorf("Nodes.Remedy = %q, want %q", info.Nodes.Remedy, want)
	}
	// The two GPU nodes share a shape, so they fold; the blocked group sorts
	// first because that is the one the operator has to read.
	if len(info.Nodes.Groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(info.Nodes.Groups))
	}
	if !info.Nodes.Groups[0].Blocked || info.Nodes.Groups[0].Count != 2 {
		t.Errorf("first group = %+v, want the blocked 2-node GPU shape", info.Nodes.Groups[0])
	}
}

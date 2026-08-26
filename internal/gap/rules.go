package gap

import (
	"fmt"
	"strings"

	"github.com/NVIDIA/aicr/pkg/measurement"
)

// Subtype names read from the fixture. These are producer-defined strings —
// see github.com/NVIDIA/aicr/pkg/measurement's catalog — not exported
// constants, so they are pinned here where they are used.
const (
	subtypeGPUHardware        = "hardware" // TypeGPU
	subtypeK8sNode            = "node"     // TypeK8s
	subtypeK8sServer          = "server"   // TypeK8s
	subtypeK8sImage           = "image"    // TypeK8s — pkg/collector/k8s/image.go
	subtypeK8sPolicy          = "policy"   // TypeK8s — pkg/collector/k8s/policy.go
	subtypeOSRelease          = "release"  // TypeOS
	subtypeNodeTopologySumary = "summary"  // TypeNodeTopology
)

// kaiSchedulerImageNames are container image names distinctive to the
// KAI-Scheduler chart (github.com/NVIDIA/KAI-Scheduler,
// deployments/kai-scheduler/values.yaml) that AICR's recipe installs as the
// "kai-scheduler" component (recipes/registry.yaml). AICR's image collector
// (pkg/collector/k8s/image.go stripRegistryPrefix) reduces every pod image
// reference to a bare <name>:<tag>, discarding the registry and repository
// path, so the plain "scheduler" image name is deliberately excluded here —
// it would also match kube-scheduler or any other pod that happens to name
// its image "scheduler". podgrouper/podgroupcontroller/queuecontroller are
// unique to this chart.
var kaiSchedulerImageNames = []string{"podgrouper", "podgroupcontroller", "queuecontroller"}

// componentGPUOperator names the AICR recipe component (recipes/registry.yaml)
// that closes gpu-driver, device-plugin, and gpu-metrics: all three are
// GPU-Operator-managed — the driver, device plugin, and DCGM exporter are
// installed and toggled together by one ClusterPolicy, not by three separate
// charts — so the Discover screen's Component field must say "gpu-operator"
// for all three, not invent a "dcgm-exporter" component the recipe catalog
// does not have.
const componentGPUOperator = "gpu-operator"

// readingTrue is the text form Scalar[bool].String() produces for a true
// Reading — Get(key).String() is how a bool-typed Data value (e.g.
// driver-loaded, gpu-present) is compared, since GetString rejects non-string
// Readings.
const readingTrue = "true"

// rule is one gap detector. Applies returns true when the capability is absent.
type rule struct {
	ID        string
	Title     string
	Detail    string
	Component string
	Applies   func(probe) bool
}

// rules covers four of the spec's five candidate gaps — GPU driver, device
// plugin, GPU-aware scheduler, GPU metrics — against the live KWOK fixture
// (internal/gap/testdata/snapshot-kwok.yaml, captured from the
// aicr-kwok-test cluster with `aicr snapshot`). Only EFA plugin is deferred:
//
//   - gpu-driver: TypeGPU's "hardware" subtype carries driver-loaded, and the
//     fixture has it false. Direct evidence.
//   - device-plugin, gpu-metrics: both are GPU Operator sub-features toggled
//     by ClusterPolicy (devicePlugin.enabled, dcgm.enabled/dcgmExporter.enabled
//     — see .agents/skills/aicr-analyzing-snapshots/SKILL.md in the aicr repo).
//     K8s.policy (pkg/collector/k8s/policy.go) is the flattened ClusterPolicy
//     spec; the fixture's policy subtype is present and empty — a positively
//     collected zero-ClusterPolicy result, not a missing measurement — so no
//     GPU Operator, hence neither sub-feature runs. Corroborated (see
//     gpuOperatorAbsent) by a non-empty K8s.image, proving the K8s collector
//     genuinely ran rather than degrading to emptyK8sMeasurement()
//     (pkg/collector/k8s/k8s.go), which zeroes policy AND image together —
//     without that corroboration, empty policy alone is ambiguous between
//     "no ClusterPolicy" and "collector never ran".
//   - gpu-scheduler: K8s.image lists every unique container image running in
//     the cluster and carries none of kai-scheduler's (see
//     kaiSchedulerImageNames) — the GPU-aware scheduler the recipe would
//     install (recipes/registry.yaml component "kai-scheduler") is not
//     running. kube-scheduler is present but is not GPU-aware.
//   - EFA plugin: fabric CAPABILITY needs a TypeNetworkTopology measurement
//     (subtypes identity/capabilities/pfs carry rdma/ib/rail data), which is
//     gated behind --discover-network (pkg/snapshotter/agent.go) and this
//     fixture never sets that flag, so TypeNetworkTopology is absent
//     entirely — not just empty. Plugin PRESENCE alone could in principle be
//     read off K8s.image the same way gpu-scheduler is, but AICR's own EFA
//     component (recipes/registry.yaml "aws-efa-k8s-device-plugin") has no
//     verified image name in this fixture's schema to key on, and getting
//     that wrong is worse than leaving it deferred to the real EKS fixture
//     (Phase 4), which will also need --discover-network for the fabric half
//     regardless.
var rules = []rule{
	{
		ID:        "gpu-driver",
		Title:     "No GPU driver installed, the kernel does not see the devices",
		Detail:    "GPU.hardware reports driver-loaded=false: the NVIDIA driver is not loaded, so nvidia-smi and the device nodes are unavailable.",
		Component: componentGPUOperator,
		Applies: func(p probe) bool {
			m := p.measurement(measurement.TypeGPU)
			if m == nil {
				return false
			}
			st := m.GetSubtype(subtypeGPUHardware)
			if st == nil || !st.Has(measurement.KeyGPUDriverLoaded) {
				return false
			}
			// driver-loaded is a bool reading, not a string one — GetString
			// would fail with ErrCodeInvalidRequest against it. Get().String()
			// round-trips any scalar Reading to its text form regardless of
			// underlying type.
			return st.Get(measurement.KeyGPUDriverLoaded).String() != readingTrue
		},
	},
	{
		ID:        "device-plugin",
		Title:     "No device plugin, Kubernetes cannot schedule nvidia.com/gpu",
		Detail:    "K8s.policy (the GPU Operator's ClusterPolicy) was collected and is empty: no ClusterPolicy exists, so the device plugin DaemonSet it manages is not running. K8s.image is non-empty, so this is a real read, not a degraded collector.",
		Component: componentGPUOperator,
		Applies:   gpuOperatorAbsent,
	},
	{
		ID:        "gpu-metrics",
		Title:     "No GPU metrics, utilization is invisible",
		Detail:    "Same evidence as device-plugin: K8s.policy is collected-empty (no ClusterPolicy) with a non-empty K8s.image corroborating a healthy collector, so the DCGM exporter — also GPU-Operator-managed via ClusterPolicy's dcgm.enabled/dcgmExporter.enabled — is not running either.",
		Component: componentGPUOperator,
		Applies:   gpuOperatorAbsent,
	},
	{
		ID:        "gpu-scheduler",
		Title:     "No GPU-aware scheduler, no gang scheduling for multi-node jobs",
		Detail:    "K8s.image lists every unique container image running in the cluster and none of them are kai-scheduler's (podgrouper, podgroupcontroller, queuecontroller). kube-scheduler is present but does not gang-schedule.",
		Component: "kai-scheduler",
		Applies:   gpuSchedulerAbsent,
	},
}

// gpuOperatorAbsent reports whether K8s.policy (the GPU Operator's flattened
// ClusterPolicy spec) was collected and came back empty, corroborated by a
// non-empty K8s.image proving the K8s collector actually ran. Both
// device-plugin and gpu-metrics are GPU-Operator-managed sub-features with no
// ClusterPolicy signal of their own beyond "no ClusterPolicy at all", so they
// share this one check. Returns false — no gap — when either subtype is
// missing entirely, or when policy is empty AND image is also empty: that
// combination is what a degraded K8s collector produces
// (pkg/collector/k8s/k8s.go emptyK8sMeasurement), and is not evidence of
// anything.
func gpuOperatorAbsent(p probe) bool {
	m := p.measurement(measurement.TypeK8s)
	if m == nil {
		return false
	}
	imageSt := m.GetSubtype(subtypeK8sImage)
	policySt := m.GetSubtype(subtypeK8sPolicy)
	if imageSt == nil || policySt == nil {
		return false
	}
	return len(imageSt.Data) > 0 && len(policySt.Data) == 0
}

// gpuSchedulerAbsent reports whether K8s.image was collected, is non-empty,
// and carries none of kaiSchedulerImageNames. An empty (or missing) image
// list means the collector found nothing to check at all, which is not
// evidence the scheduler is absent.
func gpuSchedulerAbsent(p probe) bool {
	m := p.measurement(measurement.TypeK8s)
	if m == nil {
		return false
	}
	st := m.GetSubtype(subtypeK8sImage)
	if st == nil || len(st.Data) == 0 {
		return false
	}
	for _, name := range kaiSchedulerImageNames {
		if st.Has(name) {
			return false
		}
	}
	return true
}

// providerName reads K8s.node's provider field, e.g. "kind" or "eks".
func providerName(p probe) string {
	m := p.measurement(measurement.TypeK8s)
	if m == nil {
		return ""
	}
	st := m.GetSubtype(subtypeK8sNode)
	if st == nil {
		return ""
	}
	v, err := st.GetString("provider")
	if err != nil {
		return ""
	}
	return v
}

// k8sServerVersion reads K8s.server's version field.
func k8sServerVersion(p probe) string {
	m := p.measurement(measurement.TypeK8s)
	if m == nil {
		return ""
	}
	st := m.GetSubtype(subtypeK8sServer)
	if st == nil {
		return ""
	}
	v, err := st.GetString(measurement.KeyVersion)
	if err != nil {
		return ""
	}
	return v
}

// osPrettyName reads OS.release's PRETTY_NAME field, e.g. "Ubuntu 22.04.4 LTS".
func osPrettyName(p probe) string {
	m := p.measurement(measurement.TypeOS)
	if m == nil {
		return ""
	}
	st := m.GetSubtype(subtypeOSRelease)
	if st == nil {
		return ""
	}
	v, err := st.GetString("PRETTY_NAME")
	if err != nil {
		return ""
	}
	return v
}

// nodeCount reads NodeTopology.summary's node-count field.
func nodeCount(p probe) int {
	m := p.measurement(measurement.TypeNodeTopology)
	if m == nil {
		return 0
	}
	st := m.GetSubtype(subtypeNodeTopologySumary)
	if st == nil {
		return 0
	}
	v, err := st.GetInt64("node-count")
	if err != nil {
		return 0
	}
	return int(v)
}

// totalGPUs reads GPU.hardware's gpu-count field. Zero on a snapshot with no
// GPU measurement at all, or with a measurement that never detected hardware —
// both render the same way, since neither result is a usable GPU.
func totalGPUs(p probe) int {
	m := p.measurement(measurement.TypeGPU)
	if m == nil {
		return 0
	}
	st := m.GetSubtype(subtypeGPUHardware)
	if st == nil {
		return 0
	}
	v, err := st.GetInt64(measurement.KeyGPUCount)
	if err != nil {
		return 0
	}
	return int(v)
}

// usableGPUs is the subset of totalGPUs a workload could actually schedule
// onto today: hardware present and the driver loaded. This is a lower bound
// derived from the same "hardware" subtype the gpu-driver rule reads — the
// snapshot carries no device-plugin or scheduler signal to refine it further.
func usableGPUs(p probe) int {
	total := totalGPUs(p)
	if total == 0 {
		return 0
	}
	m := p.measurement(measurement.TypeGPU)
	if m == nil {
		return 0
	}
	st := m.GetSubtype(subtypeGPUHardware)
	if st == nil {
		return 0
	}
	if !st.Has(measurement.KeyGPUPresent) || st.Get(measurement.KeyGPUPresent).String() != readingTrue {
		return 0
	}
	if !st.Has(measurement.KeyGPUDriverLoaded) || st.Get(measurement.KeyGPUDriverLoaded).String() != readingTrue {
		return 0
	}
	return total
}

// headline never emits "This is a the cluster..." — when providerName is
// unreadable it drops the article-plus-adjective clause entirely rather than
// filling it with a placeholder, since this is the first sentence a user
// reads.
// headline takes the resolved GPU total rather than reading the probe itself.
//
// It used to call totalGPUs(p) directly, which is one node's PCI probe, so on
// a two-node H100 cluster it announced "a gke cluster with 8 GPUs" for sixteen.
// Passing the resolved number in means headline and punchline cannot disagree:
// when only punchline was corrected, the screen carried "with 8 GPUs" directly
// above "16 of 16 GPUs are usable", which reads as a broken tool.
func headline(p probe, totalGPUs int) string {
	clusterPhrase := "This cluster"
	if provider := providerName(p); provider != "" {
		clusterPhrase = "This is a " + provider + " cluster"
	}
	if totalGPUs > 0 {
		return fmt.Sprintf("%s with %d GPUs.", clusterPhrase, totalGPUs)
	}
	return fmt.Sprintf("%s with %d node(s) and no GPU hardware detected.", clusterPhrase, nodeCount(p))
}

func detail(p probe) string {
	var parts []string
	if v := k8sServerVersion(p); v != "" {
		parts = append(parts, "Kubernetes "+v)
	}
	if v := osPrettyName(p); v != "" {
		parts = append(parts, v)
	}
	return strings.Join(parts, ", ")
}
